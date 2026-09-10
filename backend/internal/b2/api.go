package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ---------------------------------------------------------------- types

// UploadTarget is a one-shot credential handed to the browser. Each B2 upload
// URL serves ONE upload at a time — parallel part uploads need one target per
// concurrent worker, which is why PartUploadURLs takes a count.
type UploadTarget struct {
	UploadURL string `json:"upload_url"`
	Token     string `json:"upload_token"`
	FileID    string `json:"file_id,omitempty"`
}

type FileInfo struct {
	FileID        string            `json:"fileId"`
	FileName      string            `json:"fileName"`
	ContentLength int64             `json:"contentLength"`
	ContentSha1   string            `json:"contentSha1"`
	ContentType   string            `json:"contentType"`
	UploadTime    int64             `json:"uploadTimestamp"`
	Info          map[string]string `json:"fileInfo"`
}

// WholeSha1 returns the SHA-1 of the complete object.
//
// B2 reports contentSha1 as the literal string "none" for large files, because
// it never sees the whole byte stream in one request. The whole-file digest is
// carried in fileInfo instead, which is why StartLargeFile always sets it —
// without it the verification in spec §4.2 has nothing to check against.
func (f *FileInfo) WholeSha1() string {
	if f.ContentSha1 != "" && f.ContentSha1 != "none" {
		return f.ContentSha1
	}
	return f.Info["large_file_sha1"]
}

// ---------------------------------------------------------------- upload

// GetUploadURL mints a credential for a single small-file upload.
func (c *Client) GetUploadURL(ctx context.Context, bucketID string) (*UploadTarget, error) {
	var out struct {
		UploadURL string `json:"uploadUrl"`
		Token     string `json:"authorizationToken"`
	}
	if err := c.Call(ctx, "b2_get_upload_url", map[string]any{"bucketId": bucketID}, &out); err != nil {
		return nil, err
	}
	return &UploadTarget{UploadURL: out.UploadURL, Token: out.Token}, nil
}

// StartLargeFile opens a multipart upload.
//
// wholeSha1 is stored in fileInfo as large_file_sha1 so that the completed
// object can still be verified end to end; see FileInfo.WholeSha1.
func (c *Client) StartLargeFile(ctx context.Context, bucketID, name, contentType, wholeSha1 string) (*FileInfo, error) {
	info := map[string]string{}
	if wholeSha1 != "" {
		info["large_file_sha1"] = wholeSha1
	}
	var out FileInfo
	err := c.Call(ctx, "b2_start_large_file", map[string]any{
		"bucketId":    bucketID,
		"fileName":    name,
		"contentType": contentType,
		"fileInfo":    info,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUploadPartURL mints one part-upload credential.
func (c *Client) GetUploadPartURL(ctx context.Context, largeFileID string) (*UploadTarget, error) {
	var out struct {
		UploadURL string `json:"uploadUrl"`
		Token     string `json:"authorizationToken"`
		FileID    string `json:"fileId"`
	}
	if err := c.Call(ctx, "b2_get_upload_part_url", map[string]any{"fileId": largeFileID}, &out); err != nil {
		return nil, err
	}
	return &UploadTarget{UploadURL: out.UploadURL, Token: out.Token, FileID: out.FileID}, nil
}

// FinishLargeFile assembles the parts. partSha1s must be in part order,
// starting at part 1.
func (c *Client) FinishLargeFile(ctx context.Context, largeFileID string, partSha1s []string) (*FileInfo, error) {
	var out FileInfo
	err := c.Call(ctx, "b2_finish_large_file", map[string]any{
		"fileId":        largeFileID,
		"partSha1Array": partSha1s,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelLargeFile discards an unfinished multipart upload and its parts.
// Unfinished large files are billed until cancelled, so the orphan sweep in
// spec §7.2 depends on this.
func (c *Client) CancelLargeFile(ctx context.Context, largeFileID string) error {
	return c.Call(ctx, "b2_cancel_large_file", map[string]any{"fileId": largeFileID}, nil)
}

// PutObject uploads a small object from the server.
//
// This is the one place file bytes legitimately pass through the VPS: the
// per-file recovery sidecars of spec §4.2 and the nightly database backup of
// §7.3. Both are small and neither is on a request path.
func (c *Client) PutObject(ctx context.Context, bucketID, name, contentType string, data []byte) (*FileInfo, error) {
	sum := sha1.Sum(data)
	digest := hex.EncodeToString(sum[:])

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}

		target, err := c.GetUploadURL(ctx, bucketID)
		if err != nil {
			lastErr = err
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.UploadURL, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", target.Token)
		req.Header.Set("X-Bz-File-Name", EscapeName(name))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Bz-Content-Sha1", digest)
		req.ContentLength = int64(len(data))

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var out FileInfo
			if err := json.Unmarshal(body, &out); err != nil {
				return nil, err
			}
			return &out, nil
		}

		apiErr := decodeError(resp.StatusCode, body)
		lastErr = fmt.Errorf("upload %s: %w", name, apiErr)
		if be, ok := AsError(apiErr); ok && (be.Retryable() || be.ExpiredAuth()) {
			// A stale upload URL is retried with a fresh one, which the next
			// loop iteration fetches.
			continue
		}
		return nil, lastErr
	}
	return nil, lastErr
}

// ---------------------------------------------------------------- read

func (c *Client) GetFileInfo(ctx context.Context, fileID string) (*FileInfo, error) {
	var out FileInfo
	if err := c.Call(ctx, "b2_get_file_info", map[string]any{"fileId": fileID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDownloadAuthorization issues a token covering every object under prefix.
//
// This is a Class C transaction. Calling it per file costs one transaction per
// thumbnail and exhausts the 2,500/day free tier in roughly fifty gallery page
// loads; calling it once per prefix for the maximum seven days costs one per
// week and keeps the query string stable long enough for Cloudflare to cache
// (spec §2.4).
func (c *Client) GetDownloadAuthorization(ctx context.Context, bucketID, prefix string, validSeconds int) (string, error) {
	var out struct {
		Token string `json:"authorizationToken"`
	}
	err := c.Call(ctx, "b2_get_download_authorization", map[string]any{
		"bucketId":               bucketID,
		"fileNamePrefix":         prefix,
		"validDurationInSeconds": validSeconds,
	}, &out)
	if err != nil {
		return "", err
	}
	return out.Token, nil
}

// ListNames pages through object names under a prefix. Used only by
// `cloudapp reindex --from-b2` (spec §7.3) — it is a Class C transaction and
// has no place on a request path.
func (c *Client) ListNames(ctx context.Context, bucketID, prefix, startFrom string, limit int) ([]FileInfo, string, error) {
	var out struct {
		Files        []FileInfo `json:"files"`
		NextFileName string     `json:"nextFileName"`
	}
	body := map[string]any{
		"bucketId":     bucketID,
		"prefix":       prefix,
		"maxFileCount": limit,
	}
	if startFrom != "" {
		body["startFileName"] = startFrom
	}
	if err := c.Call(ctx, "b2_list_file_names", body, &out); err != nil {
		return nil, "", err
	}
	return out.Files, out.NextFileName, nil
}

// ---------------------------------------------------------------- delete

// DeleteFileVersion removes one version permanently.
//
// This is what actually stops the billing. A soft-deleted row whose B2 object
// is never deleted is paid for forever, which is the single most expensive
// omission the original plan had.
func (c *Client) DeleteFileVersion(ctx context.Context, name, fileID string) error {
	err := c.Call(ctx, "b2_delete_file_version", map[string]any{
		"fileName": name,
		"fileId":   fileID,
	}, nil)
	// Already gone is the desired end state, not a failure. The purge job must
	// be able to run twice over the same row without wedging.
	if be, ok := AsError(err); ok && (be.Code == "file_not_present" || be.Status == http.StatusNotFound) {
		return nil
	}
	return err
}

// GetObject downloads an object using the account authorization.
//
// Used only by maintenance paths — reading recovery sidecars during a reindex,
// and the fallback thumbnail worker fetching what it needs. Normal downloads go
// straight from the browser to storage (decision D1) and never pass through here.
func (c *Client) GetObject(ctx context.Context, bucketName, name string) ([]byte, error) {
	auth, err := c.Auth(ctx)
	if err != nil {
		return nil, err
	}

	url := auth.DownloadURL + "/file/" + bucketName + "/" + EscapeName(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth.Token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %w", name, decodeError(resp.StatusCode, body))
	}
	return body, nil
}
