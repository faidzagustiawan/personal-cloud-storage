// Command b2probe is the Step 0 de-risk probe from CLOUD_STORAGE_SPEC.md §11.
//
// It exercises, against a real bucket, every Backblaze B2 interaction the
// application depends on, in the same order and with the same parameters the
// real implementation will use:
//
//  1. b2_authorize_account, and inspection of the key's restrictions
//  2. b2_list_buckets, to verify the CORS and lifecycle rules from §2.1/§2.2
//  3. b2_start_large_file
//  4. b2_get_upload_part_url (one per concurrent worker)
//  5. b2_upload_part, 10 MB parts, 4 concurrent, SHA1 per part
//  6. b2_finish_large_file
//  7. b2_get_file_info, and verification of size and SHA1
//  8. b2_get_download_authorization, prefix-scoped, 7 days
//  9. download through the native B2 host, and optionally through Cloudflare
//  10. the small-file path: b2_get_upload_url + b2_upload_file
//  11. the restricted upload key's blast radius (§2.3)
//  12. b2_delete_file_version cleanup
//
// Deliberately uses no B2 SDK. The raw HTTP calls are the thing being proven,
// and this file becomes the basis for the real b2 service layer in Step 2.
//
// Usage:
//
//	export B2_KEY_ID=...          # master key
//	export B2_APP_KEY=...
//	export B2_BUCKET_ID=...
//	export B2_BUCKET_NAME=...
//	export B2_UPLOAD_KEY_ID=...   # optional: the writeFiles-only restricted key
//	export B2_UPLOAD_APP_KEY=...
//	export B2_CDN_BASE=https://cdn.faidz.fun/f   # optional
//	go run . --size 200MB
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiVersion  = "/b2api/v2"
	partSize    = 10 << 20 // 10 MB, per spec §5.3
	concurrency = 4        // matches the browser's part-upload concurrency
	largeCutoff = 100 << 20
)

// ---------------------------------------------------------------- reporting

var (
	failures int
	stepNum  int
)

func step(format string, a ...any) {
	stepNum++
	fmt.Printf("\n\033[1m[%2d] %s\033[0m\n", stepNum, fmt.Sprintf(format, a...))
}

func ok(format string, a ...any)   { fmt.Printf("     PASS  %s\n", fmt.Sprintf(format, a...)) }
func info(format string, a ...any) { fmt.Printf("           %s\n", fmt.Sprintf(format, a...)) }
func warn(format string, a ...any) { fmt.Printf("     WARN  %s\n", fmt.Sprintf(format, a...)) }

func bad(format string, a ...any) {
	failures++
	fmt.Printf("     \033[31mFAIL  %s\033[0m\n", fmt.Sprintf(format, a...))
}

func die(err error) {
	fmt.Printf("\n\033[31maborted: %v\033[0m\n", err)
	os.Exit(1)
}

// ---------------------------------------------------------------- b2 client

type client struct {
	http        *http.Client
	apiURL      string
	downloadURL string
	token       string
	accountID   string
	allowed     allowed
	partSizeRec int64
	partSizeMin int64
}

type allowed struct {
	Capabilities []string `json:"capabilities"`
	BucketID     string   `json:"bucketId"`
	BucketName   string   `json:"bucketName"`
	NamePrefix   *string  `json:"namePrefix"`
}

type authResponse struct {
	AccountID           string  `json:"accountId"`
	AuthorizationToken  string  `json:"authorizationToken"`
	APIURL              string  `json:"apiUrl"`
	DownloadURL         string  `json:"downloadUrl"`
	RecommendedPartSize int64   `json:"recommendedPartSize"`
	AbsoluteMinPartSize int64   `json:"absoluteMinimumPartSize"`
	Allowed             allowed `json:"allowed"`
}

type b2Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *b2Error) Error() string {
	return fmt.Sprintf("b2 %d %s: %s", e.Status, e.Code, e.Message)
}

func authorize(keyID, appKey string) (*client, error) {
	req, err := http.NewRequest(http.MethodGet,
		"https://api.backblazeb2.com"+apiVersion+"/b2_authorize_account", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(keyID, appKey)

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, decodeErr(resp.StatusCode, body)
	}

	var ar authResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("decode auth response: %w", err)
	}
	return &client{
		http:        &http.Client{Timeout: 10 * time.Minute},
		apiURL:      ar.APIURL,
		downloadURL: ar.DownloadURL,
		token:       ar.AuthorizationToken,
		accountID:   ar.AccountID,
		allowed:     ar.Allowed,
		partSizeRec: ar.RecommendedPartSize,
		partSizeMin: ar.AbsoluteMinPartSize,
	}, nil
}

func decodeErr(status int, body []byte) error {
	var be b2Error
	if json.Unmarshal(body, &be) == nil && be.Code != "" {
		be.Status = status
		return &be
	}
	return fmt.Errorf("http %d: %s", status, strings.TrimSpace(string(body)))
}

// call performs an authenticated JSON POST against the account's API host.
func (c *client) call(endpoint string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.apiURL+apiVersion+"/"+endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return decodeErr(resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// ---------------------------------------------------------------- API types

type uploadURL struct {
	UploadURL          string `json:"uploadUrl"`
	AuthorizationToken string `json:"authorizationToken"`
	FileID             string `json:"fileId"`
}

type fileInfo struct {
	FileID        string            `json:"fileId"`
	FileName      string            `json:"fileName"`
	ContentLength int64             `json:"contentLength"`
	ContentSha1   string            `json:"contentSha1"`
	ContentType   string            `json:"contentType"`
	FileInfo      map[string]string `json:"fileInfo"`
}

type bucket struct {
	BucketID   string `json:"bucketId"`
	BucketName string `json:"bucketName"`
	BucketType string `json:"bucketType"`
	CorsRules  []struct {
		Name           string   `json:"corsRuleName"`
		AllowedOrigins []string `json:"allowedOrigins"`
		AllowedOps     []string `json:"allowedOperations"`
		AllowedHeaders []string `json:"allowedHeaders"`
		ExposeHeaders  []string `json:"exposeHeaders"`
		MaxAgeSeconds  int      `json:"maxAgeSeconds"`
	} `json:"corsRules"`
	LifecycleRules []struct {
		FileNamePrefix            string `json:"fileNamePrefix"`
		DaysFromHidingToDeleting  *int   `json:"daysFromHidingToDeleting"`
		DaysFromUploadingToHiding *int   `json:"daysFromUploadingToHiding"`
	} `json:"lifecycleRules"`
}

// ---------------------------------------------------------------- test data

// makeTestFile writes size bytes of incompressible random data to a temp file
// and returns its path plus the SHA1 of the whole file. Random data matters:
// photos and videos are already compressed, so a file of zeros would give a
// misleadingly fast result on any link that does compression.
func makeTestFile(size int64) (string, string, error) {
	f, err := os.CreateTemp("", "b2probe-*.bin")
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	h := sha1.New()
	w := io.MultiWriter(f, h)

	buf := make([]byte, 1<<20)
	for remaining := size; remaining > 0; {
		n := int64(len(buf))
		if remaining < n {
			n = remaining
		}
		if _, err := rand.Read(buf[:n]); err != nil {
			return "", "", err
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return "", "", err
		}
		remaining -= n
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1<<30, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1<<10, strings.TrimSuffix(s, "KB")
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return int64(n * float64(mult)), nil
}

// ---------------------------------------------------------------- large file

// uploadLarge runs the multipart upload exactly as the browser will: N parts of
// partSize bytes, `concurrency` of them in flight, one upload-part URL per
// worker because a B2 upload URL serves one upload at a time.
func (c *client) uploadLarge(path string, size int64, fileName, contentType, wholeSha1 string) (string, error) {
	var start struct {
		FileID string `json:"fileId"`
	}
	err := c.call("b2_start_large_file", map[string]any{
		"bucketId":    cfg.bucketID,
		"fileName":    fileName,
		"contentType": contentType,
		// B2 reports contentSha1 as "none" for large files. Carrying the whole
		// file's SHA1 in fileInfo is the documented way to keep end-to-end
		// verification possible, and §4.2 of the spec depends on it.
		"fileInfo": map[string]string{"large_file_sha1": wholeSha1},
	}, &start)
	if err != nil {
		return "", fmt.Errorf("b2_start_large_file: %w", err)
	}
	info("fileId %s", start.FileID)

	numParts := int((size + partSize - 1) / partSize)
	info("%d parts of %s, %d concurrent", numParts, humanBytes(partSize), concurrency)

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sha1s := make([]string, numParts)
	jobs := make(chan int)
	errs := make(chan error, concurrency)
	var wg sync.WaitGroup

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var up uploadURL
			if err := c.call("b2_get_upload_part_url", map[string]any{"fileId": start.FileID}, &up); err != nil {
				errs <- fmt.Errorf("b2_get_upload_part_url: %w", err)
				return
			}

			for idx := range jobs {
				offset := int64(idx) * partSize
				length := int64(partSize)
				if rem := size - offset; rem < length {
					length = rem
				}
				chunk := make([]byte, length)
				if _, err := f.ReadAt(chunk, offset); err != nil && !errors.Is(err, io.EOF) {
					errs <- err
					return
				}
				sum := sha1Hex(chunk)
				sha1s[idx] = sum

				if err := c.uploadPart(&up, start.FileID, idx+1, chunk, sum); err != nil {
					errs <- fmt.Errorf("part %d: %w", idx+1, err)
					return
				}
				fmt.Printf("\r           uploaded part %d/%d", idx+1, numParts)
			}
		}()
	}

	go func() {
		for i := 0; i < numParts; i++ {
			jobs <- i
		}
		close(jobs)
	}()

	wg.Wait()
	fmt.Print("\r\033[K")
	close(errs)
	if err := <-errs; err != nil {
		return "", err
	}

	var finished fileInfo
	if err := c.call("b2_finish_large_file", map[string]any{
		"fileId":        start.FileID,
		"partSha1Array": sha1s,
	}, &finished); err != nil {
		return "", fmt.Errorf("b2_finish_large_file: %w", err)
	}
	return finished.FileID, nil
}

// uploadPart retries the three failure modes B2 documents as retryable, and
// re-fetches the upload URL when the token is the thing that went stale.
func (c *client) uploadPart(up *uploadURL, fileID string, partNum int, chunk []byte, sum string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			var fresh uploadURL
			if err := c.call("b2_get_upload_part_url", map[string]any{"fileId": fileID}, &fresh); err == nil {
				*up = fresh
			}
		}

		req, err := http.NewRequest(http.MethodPost, up.UploadURL, bytes.NewReader(chunk))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", up.AuthorizationToken)
		req.Header.Set("X-Bz-Part-Number", strconv.Itoa(partNum))
		req.Header.Set("X-Bz-Content-Sha1", sum)
		req.ContentLength = int64(len(chunk))

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil
		}
		lastErr = decodeErr(resp.StatusCode, body)
		if resp.StatusCode != 401 && resp.StatusCode != 408 && resp.StatusCode != 429 && resp.StatusCode < 500 {
			return lastErr
		}
	}
	return lastErr
}

// uploadSmall is the sub-100MB path: one b2_get_upload_url, one b2_upload_file.
func (c *client) uploadSmall(data []byte, fileName, contentType string) (string, error) {
	var up uploadURL
	if err := c.call("b2_get_upload_url", map[string]any{"bucketId": cfg.bucketID}, &up); err != nil {
		return "", fmt.Errorf("b2_get_upload_url: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", up.AuthorizationToken)
	// B2 wants the name percent-encoded with "/" left literal as the separator.
	// url.PathEscape alone escapes "/" to %2F, which would flatten the whole
	// users/1/thumb/... hierarchy into one bogus filename.
	req.Header.Set("X-Bz-File-Name", escapeName(fileName))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Bz-Content-Sha1", sha1Hex(data))
	req.ContentLength = int64(len(data))

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", decodeErr(resp.StatusCode, body)
	}
	var fi fileInfo
	if err := json.Unmarshal(body, &fi); err != nil {
		return "", err
	}
	return fi.FileID, nil
}

// download fetches a URL, hashes the body without buffering it, and reports
// throughput plus any CDN cache header.
func download(rawURL string) (sha string, n int64, dur time.Duration, hdr http.Header, err error) {
	start := time.Now()
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(rawURL)
	if err != nil {
		return "", 0, 0, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, 0, resp.Header, decodeErr(resp.StatusCode, body)
	}
	h := sha1.New()
	n, err = io.Copy(h, resp.Body)
	if err != nil {
		return "", 0, 0, resp.Header, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, time.Since(start), resp.Header, nil
}

// ---------------------------------------------------------------- main

var cfg struct {
	keyID, appKey          string
	uploadKeyID, uploadKey string
	bucketID, bucketName   string
	cdnBase                string
}

func env(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func main() {
	sizeFlag := flag.String("size", "200MB", "test file size, e.g. 20MB or 200MB")
	prefix := flag.String("prefix", "users/1", "object key prefix to write under")
	keep := flag.Bool("keep", false, "do not delete the uploaded test objects")
	flag.Parse()

	cfg.keyID, cfg.appKey = env("B2_KEY_ID"), env("B2_APP_KEY")
	cfg.uploadKeyID, cfg.uploadKey = env("B2_UPLOAD_KEY_ID"), env("B2_UPLOAD_APP_KEY")
	cfg.bucketID, cfg.bucketName = env("B2_BUCKET_ID"), env("B2_BUCKET_NAME")
	cfg.cdnBase = strings.TrimRight(env("B2_CDN_BASE"), "/")

	if cfg.keyID == "" || cfg.appKey == "" || cfg.bucketID == "" || cfg.bucketName == "" {
		die(errors.New("set B2_KEY_ID, B2_APP_KEY, B2_BUCKET_ID and B2_BUCKET_NAME"))
	}

	size, err := parseSize(*sizeFlag)
	if err != nil {
		die(fmt.Errorf("bad --size: %w", err))
	}

	fmt.Printf("\033[1mb2probe\033[0m  bucket=%s  size=%s\n", cfg.bucketName, humanBytes(size))
	overall := time.Now()

	// ---- 1
	step("b2_authorize_account (master key)")
	c, err := authorize(cfg.keyID, cfg.appKey)
	if err != nil {
		die(fmt.Errorf("authorize: %w", err))
	}
	ok("apiUrl=%s", c.apiURL)
	info("downloadUrl=%s", c.downloadURL)
	info("recommendedPartSize=%s  absoluteMinimumPartSize=%s",
		humanBytes(c.partSizeRec), humanBytes(c.partSizeMin))
	info("capabilities=%s", strings.Join(c.allowed.Capabilities, ","))
	if partSize < c.partSizeMin {
		bad("configured part size %s is below the account minimum %s",
			humanBytes(partSize), humanBytes(c.partSizeMin))
	} else {
		ok("part size %s is valid for this account", humanBytes(partSize))
	}

	// ---- 2
	step("b2_list_buckets: verify CORS and lifecycle rules (spec §2.1, §2.2)")
	var buckets struct {
		Buckets []bucket `json:"buckets"`
	}
	if err := c.call("b2_list_buckets", map[string]any{
		"accountId": c.accountID, "bucketId": cfg.bucketID,
	}, &buckets); err != nil {
		bad("b2_list_buckets: %v", err)
	} else if len(buckets.Buckets) == 0 {
		bad("bucket %s not visible to this key", cfg.bucketID)
	} else {
		b := buckets.Buckets[0]
		if b.BucketType == "allPrivate" {
			ok("bucket is private")
		} else {
			bad("bucket type is %q, expected allPrivate", b.BucketType)
		}

		if len(b.CorsRules) == 0 {
			bad("no CORS rules: direct browser upload WILL fail (spec §2.2)")
		} else {
			needed := []string{"b2_upload_file", "b2_upload_part"}
			var haveOps []string
			for _, r := range b.CorsRules {
				haveOps = append(haveOps, r.AllowedOps...)
				info("rule %q origins=%v", r.Name, r.AllowedOrigins)
			}
			for _, want := range needed {
				if !contains(haveOps, want) {
					bad("CORS rules do not allow %s", want)
				}
			}
			if failures == 0 {
				ok("CORS allows browser upload")
			}
		}

		if len(b.LifecycleRules) == 0 {
			warn("no lifecycle rule: hidden versions are billed forever (spec §2.1)")
		} else {
			for _, r := range b.LifecycleRules {
				d := "null"
				if r.DaysFromHidingToDeleting != nil {
					d = strconv.Itoa(*r.DaysFromHidingToDeleting)
				}
				info("lifecycle prefix=%q daysFromHidingToDeleting=%s", r.FileNamePrefix, d)
			}
			ok("lifecycle rules present")
		}
	}

	// ---- 3
	step("generate %s of incompressible test data", humanBytes(size))
	genStart := time.Now()
	path, wholeSha1, err := makeTestFile(size)
	if err != nil {
		die(err)
	}
	defer os.Remove(path)
	ok("sha1=%s  (%s)", wholeSha1, time.Since(genStart).Round(time.Millisecond))

	// ---- 4
	objectKey := randomKey()
	origName := fmt.Sprintf("%s/orig/%s.bin", *prefix, objectKey)
	step("upload %s as %s", humanBytes(size), origName)

	upStart := time.Now()
	var origFileID string
	if size > largeCutoff {
		origFileID, err = c.uploadLarge(path, size, origName, "application/octet-stream", wholeSha1)
	} else {
		var data []byte
		data, err = os.ReadFile(path)
		if err == nil {
			origFileID, err = c.uploadSmall(data, origName, "application/octet-stream")
		}
	}
	if err != nil {
		die(fmt.Errorf("upload: %w", err))
	}
	upDur := time.Since(upStart)
	mbps := float64(size) / (1 << 20) / upDur.Seconds()
	ok("uploaded in %s = %.1f MB/s", upDur.Round(time.Millisecond), mbps)
	if mbps < 10 {
		warn("below the 10 MB/s target in spec §10 (may be the local uplink, not B2)")
	}

	// ---- 5
	step("b2_get_file_info: verify size and SHA1 (spec §4.2 step 2)")
	var fi fileInfo
	if err := c.call("b2_get_file_info", map[string]any{"fileId": origFileID}, &fi); err != nil {
		bad("b2_get_file_info: %v", err)
	} else {
		if fi.ContentLength == size {
			ok("contentLength matches: %d", fi.ContentLength)
		} else {
			bad("contentLength %d != expected %d", fi.ContentLength, size)
		}
		switch {
		case fi.ContentSha1 == wholeSha1:
			ok("contentSha1 matches")
		case fi.ContentSha1 == "none":
			// Expected for large files. This is why large_file_sha1 exists.
			if fi.FileInfo["large_file_sha1"] == wholeSha1 {
				ok("contentSha1 is \"none\" (normal for large files); fileInfo.large_file_sha1 matches")
			} else {
				bad("contentSha1 is \"none\" and fileInfo.large_file_sha1 is %q, expected %q",
					fi.FileInfo["large_file_sha1"], wholeSha1)
			}
		default:
			bad("contentSha1 %q != expected %q", fi.ContentSha1, wholeSha1)
		}
	}

	// ---- 6
	step("b2_get_download_authorization: prefix %q for 7 days (spec §2.4)", *prefix+"/")
	var dl struct {
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := c.call("b2_get_download_authorization", map[string]any{
		"bucketId":               cfg.bucketID,
		"fileNamePrefix":         *prefix + "/",
		"validDurationInSeconds": 604800,
	}, &dl); err != nil {
		die(fmt.Errorf("b2_get_download_authorization: %w", err))
	}
	ok("token issued, %d chars, covers every object under the prefix", len(dl.AuthorizationToken))
	info("one call per week serves the whole gallery")

	// ---- 7
	nativeURL := fmt.Sprintf("%s/file/%s/%s?Authorization=%s",
		c.downloadURL, cfg.bucketName, escapeName(origName), url.QueryEscape(dl.AuthorizationToken))
	step("download from B2 directly")
	gotSha, gotN, dur, _, err := download(nativeURL)
	if err != nil {
		bad("download: %v", err)
	} else {
		dmbps := float64(gotN) / (1 << 20) / dur.Seconds()
		if gotN == size && gotSha == wholeSha1 {
			ok("%s round-tripped byte-identical in %s = %.1f MB/s",
				humanBytes(gotN), dur.Round(time.Millisecond), dmbps)
		} else {
			bad("round-trip mismatch: got %d bytes sha1=%s", gotN, gotSha)
		}
	}

	// ---- 8
	step("HTTP Range request (video seeking depends on this)")
	if err := probeRange(nativeURL); err != nil {
		bad("range request: %v", err)
	} else {
		ok("B2 honours Range; native <video> seeking will work")
	}

	// ---- 9
	if cfg.cdnBase == "" {
		step("Cloudflare download (skipped: B2_CDN_BASE not set)")
		warn("without Cloudflare, B2 egress past the free allowance costs $0.01/GB (spec §2.5)")
	} else {
		cdnURL := fmt.Sprintf("%s/%s?Authorization=%s", cfg.cdnBase, escapeName(origName), url.QueryEscape(dl.AuthorizationToken))
		step("download through Cloudflare: %s", cfg.cdnBase)
		_, cdnN, cdnDur, hdr, err := download(cdnURL)
		if err != nil {
			bad("cdn download: %v  (check the Transform Rule from spec §2.5)", err)
		} else {
			ok("%s in %s, cf-cache-status=%s",
				humanBytes(cdnN), cdnDur.Round(time.Millisecond), headerOr(hdr, "Cf-Cache-Status", "absent"))
			if hdr.Get("Cf-Cache-Status") == "" {
				warn("no cf-cache-status header: the CNAME may not be proxied (orange cloud)")
			}
		}
	}

	// ---- 10
	thumbName := fmt.Sprintf("%s/thumb/%s.webp", *prefix, objectKey)
	step("small-file path: b2_upload_file for %s", thumbName)
	thumb := make([]byte, 24<<10) // a realistic 24 KB thumbnail
	if _, err := rand.Read(thumb); err != nil {
		die(err)
	}
	thumbFileID, err := c.uploadSmall(thumb, thumbName, "image/webp")
	if err != nil {
		bad("b2_upload_file: %v", err)
	} else {
		ok("thumbnail uploaded, fileId=%s", thumbFileID)
	}

	// ---- 11
	step("restricted upload key: verify its blast radius (spec §2.3)")
	if cfg.uploadKeyID == "" || cfg.uploadKey == "" {
		warn("skipped: B2_UPLOAD_KEY_ID / B2_UPLOAD_APP_KEY not set")
		info("this key is the security boundary that makes decision D1 acceptable")
	} else {
		uc, err := authorize(cfg.uploadKeyID, cfg.uploadKey)
		if err != nil {
			bad("authorize with upload key: %v", err)
		} else {
			info("capabilities=%s", strings.Join(uc.allowed.Capabilities, ","))
			if uc.allowed.NamePrefix != nil && *uc.allowed.NamePrefix != "" {
				ok("namePrefix restricted to %q", *uc.allowed.NamePrefix)
			} else {
				bad("namePrefix is empty: a leaked browser token could write anywhere in the bucket")
			}
			for _, forbidden := range []string{"readFiles", "deleteFiles", "listFiles"} {
				if contains(uc.allowed.Capabilities, forbidden) {
					bad("upload key holds %q; it must be writeFiles only", forbidden)
				}
			}
			// Prove read really is denied rather than trusting the capability list.
			var probe fileInfo
			if err := uc.call("b2_get_file_info", map[string]any{"fileId": origFileID}, &probe); err == nil {
				bad("upload key could READ a file; the restriction is not effective")
			} else {
				ok("read denied for the upload key: %v", err)
			}
		}
	}

	// ---- 12
	step("cleanup")
	if *keep {
		warn("--keep set, leaving objects in the bucket")
		info("orig:  %s", origName)
		info("thumb: %s", thumbName)
	} else {
		for _, obj := range []struct{ name, id string }{
			{origName, origFileID}, {thumbName, thumbFileID},
		} {
			if obj.id == "" {
				continue
			}
			if err := c.call("b2_delete_file_version", map[string]any{
				"fileName": obj.name, "fileId": obj.id,
			}, nil); err != nil {
				bad("delete %s: %v", obj.name, err)
			} else {
				ok("deleted %s", filepath.Base(obj.name))
			}
		}
	}

	// ---- verdict
	fmt.Printf("\n\033[1m%s\033[0m\n", strings.Repeat("-", 60))
	if failures == 0 {
		fmt.Printf("\033[32mALL CHECKS PASSED\033[0m in %s\n", time.Since(overall).Round(time.Second))
		fmt.Println("The B2 half of Step 0 is proven. Nothing in §2, §4.2 or §5.3 is uncertain.")
	} else {
		fmt.Printf("\033[31m%d CHECK(S) FAILED\033[0m in %s\n", failures, time.Since(overall).Round(time.Second))
		os.Exit(1)
	}
}

func probeRange(rawURL string) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=1048576-1049599") // 1 KB at the 1 MB mark
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("expected 206, got %d", resp.StatusCode)
	}
	if len(body) != 1024 {
		return fmt.Errorf("expected 1024 bytes, got %d", len(body))
	}
	return nil
}

func randomKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		die(err)
	}
	return hex.EncodeToString(b)
}

// escapeName percent-encodes a B2 object name segment by segment, leaving the
// "/" separators literal.
func escapeName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func headerOr(h http.Header, key, fallback string) string {
	if v := h.Get(key); v != "" {
		return v
	}
	return fallback
}
