package b2

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudapp/internal/config"
	"cloudapp/internal/store"
)

// The real bucket is exercised by tools/b2probe. These tests cover what a live
// bucket cannot conveniently be made to do on demand: expired tokens, 503s,
// and proof that the caching actually prevents repeat transactions — which is
// the difference between one Class C call a week and one per gallery page load.

// ---------------------------------------------------------------- fake B2

type fakeB2 struct {
	srv *httptest.Server

	mu        sync.Mutex
	authCalls int
	calls     map[string]int
	// handlers may override the default response for one endpoint.
	handlers map[string]func(w http.ResponseWriter, body map[string]any)
}

func newFakeB2(t *testing.T) *fakeB2 {
	t.Helper()
	f := &fakeB2{
		calls:    map[string]int{},
		handlers: map[string]func(http.ResponseWriter, map[string]any){},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(apiVersion+"/b2_authorize_account", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authCalls++
		f.mu.Unlock()

		keyID, _, _ := r.BasicAuth()
		allowed := Allowed{
			Capabilities: []string{"listFiles", "readFiles", "writeFiles", "deleteFiles", "shareFiles"},
			BucketID:     "bucket-1",
			BucketName:   "faidz-cloud",
		}
		if keyID == "upload-key" {
			prefix := "users/1/"
			allowed = Allowed{Capabilities: []string{"writeFiles"}, BucketID: "bucket-1", NamePrefix: &prefix}
		}
		writeJSON(w, http.StatusOK, authResponse{
			AccountID:           "acct",
			AuthorizationToken:  "auth-" + keyID,
			APIURL:              f.srv.URL,
			DownloadURL:         f.srv.URL,
			RecommendedPartSize: 100 << 20,
			AbsoluteMinPartSize: 5 << 20,
			Allowed:             allowed,
		})
	})

	mux.HandleFunc(apiVersion+"/", func(w http.ResponseWriter, r *http.Request) {
		endpoint := strings.TrimPrefix(r.URL.Path, apiVersion+"/")

		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			json.Unmarshal(raw, &body)
		}

		f.mu.Lock()
		f.calls[endpoint]++
		h := f.handlers[endpoint]
		f.mu.Unlock()

		if h != nil {
			h(w, body)
			return
		}
		switch endpoint {
		case "b2_get_download_authorization":
			writeJSON(w, http.StatusOK, map[string]string{"authorizationToken": "dl-token"})
		case "b2_get_upload_url", "b2_get_upload_part_url":
			writeJSON(w, http.StatusOK, map[string]string{
				"uploadUrl": f.srv.URL + "/upload", "authorizationToken": "up-token", "fileId": "large-1",
			})
		case "b2_start_large_file":
			writeJSON(w, http.StatusOK, map[string]string{"fileId": "large-1"})
		default:
			writeJSON(w, http.StatusOK, map[string]string{})
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeB2) on(endpoint string, h func(http.ResponseWriter, map[string]any)) {
	f.mu.Lock()
	f.handlers[endpoint] = h
	f.mu.Unlock()
}

func (f *fakeB2) count(endpoint string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[endpoint]
}

func (f *fakeB2) auths() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authCalls
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func b2err(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, Error{Status: status, Code: code, Message: message})
}

// ---------------------------------------------------------------- fixtures

func testService(t *testing.T, f *fakeB2, withUploadKey bool, cdn string) *Service {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	return testServiceWithStore(t, f, st, withUploadKey, cdn)
}

func testServiceWithStore(t *testing.T, f *fakeB2, st *store.Store, withUploadKey bool, cdn string) *Service {
	t.Helper()

	cfg := &config.Config{
		B2KeyID: "master-key", B2AppKey: "secret",
		B2BucketID: "bucket-1", B2BucketName: "faidz-cloud",
		CDNBase: cdn,
	}
	if withUploadKey {
		cfg.B2UploadKeyID, cfg.B2UploadKey = "upload-key", "secret"
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewService(cfg, st, log, false)
	s.master.authBase = f.srv.URL
	if s.upload != nil {
		s.upload.authBase = f.srv.URL
	}
	return s
}

func fastBackoff(t *testing.T) {
	t.Helper()
	original := backoff
	backoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoff = original })
}

// ---------------------------------------------------------------- naming

func TestEscapeNameKeepsSeparators(t *testing.T) {
	cases := map[string]string{
		"users/1/orig/abc.jpg":   "users/1/orig/abc.jpg",
		"users/1/orig/a b.jpg":   "users/1/orig/a%20b.jpg",
		"users/1/orig/a?b#c.jpg": "users/1/orig/a%3Fb%23c.jpg",
	}
	for in, want := range cases {
		if got := EscapeName(in); got != want {
			t.Errorf("EscapeName(%q) = %q, want %q", in, got, want)
		}
	}
	// The specific failure this guards: escaping the whole name at once turns
	// every separator into %2F and flattens the hierarchy.
	if strings.Contains(EscapeName("users/1/orig/abc.jpg"), "%2F") {
		t.Error("path separators were escaped; nested object keys would break")
	}
}

func TestObjectLayout(t *testing.T) {
	if got := OrigName(1, "deadbeef", "HEIC"); got != "users/1/orig/deadbeef.heic" {
		t.Errorf("OrigName = %q", got)
	}
	if got := OrigName(1, "deadbeef", ".JPG"); got != "users/1/orig/deadbeef.jpg" {
		t.Errorf("OrigName with dotted ext = %q", got)
	}
	if got := ThumbName(2, "cafe"); got != "users/2/thumb/cafe.webp" {
		t.Errorf("ThumbName = %q", got)
	}
	if got := MetaName(2, "cafe"); got != "users/2/meta/cafe.json" {
		t.Errorf("MetaName = %q", got)
	}

	// The extension is the one part of an object key derived from a
	// client-supplied filename, so it must not be able to add a path segment
	// or a relative component.
	for _, hostile := range []string{"../../../etc/passwd", "jpg/../..", ".jpg?x=1", `j\pg`} {
		got := OrigName(1, "key", hostile)
		if strings.Contains(got, "..") || strings.Count(got, "/") != 3 {
			t.Errorf("extension %q escaped its position: %q", hostile, got)
		}
		if !strings.HasPrefix(got, "users/1/orig/key.") {
			t.Errorf("extension %q corrupted the key: %q", hostile, got)
		}
	}
}

func TestWholeSha1FallsBackToLargeFileInfo(t *testing.T) {
	small := &FileInfo{ContentSha1: "abc123"}
	if small.WholeSha1() != "abc123" {
		t.Error("small file should use contentSha1")
	}

	// B2 reports "none" for large files; the digest lives in fileInfo.
	large := &FileInfo{ContentSha1: "none", Info: map[string]string{"large_file_sha1": "def456"}}
	if large.WholeSha1() != "def456" {
		t.Errorf("large file should fall back to large_file_sha1, got %q", large.WholeSha1())
	}
}

// ---------------------------------------------------------------- client behaviour

func TestAuthorizationIsCached(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, true, "")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.master.GetFileInfo(ctx, "f1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := f.auths(); got != 1 {
		t.Errorf("authorize called %d times, want 1 — the cache is not working", got)
	}
}

func TestReauthorizesOnExpiredToken(t *testing.T) {
	fastBackoff(t)
	f := newFakeB2(t)
	s := testService(t, f, false, "")

	var n int
	f.on("b2_get_file_info", func(w http.ResponseWriter, _ map[string]any) {
		n++
		if n == 1 {
			b2err(w, http.StatusUnauthorized, "expired_auth_token", "token expired")
			return
		}
		writeJSON(w, http.StatusOK, FileInfo{FileID: "f1", ContentLength: 10})
	})

	info, err := s.master.GetFileInfo(context.Background(), "f1")
	if err != nil {
		t.Fatalf("expected recovery after re-auth, got %v", err)
	}
	if info.FileID != "f1" {
		t.Errorf("unexpected file: %+v", info)
	}
	if got := f.auths(); got != 2 {
		t.Errorf("authorize called %d times, want 2 (initial + refresh)", got)
	}
}

func TestRetriesTransientFailure(t *testing.T) {
	fastBackoff(t)
	f := newFakeB2(t)
	s := testService(t, f, false, "")

	var n int
	f.on("b2_get_file_info", func(w http.ResponseWriter, _ map[string]any) {
		n++
		if n < 3 {
			b2err(w, http.StatusServiceUnavailable, "service_unavailable", "try later")
			return
		}
		writeJSON(w, http.StatusOK, FileInfo{FileID: "f1"})
	})

	if _, err := s.master.GetFileInfo(context.Background(), "f1"); err != nil {
		t.Fatalf("should have recovered on the third attempt: %v", err)
	}
}

func TestDoesNotRetryClientError(t *testing.T) {
	fastBackoff(t)
	f := newFakeB2(t)
	s := testService(t, f, false, "")

	f.on("b2_get_file_info", func(w http.ResponseWriter, _ map[string]any) {
		b2err(w, http.StatusBadRequest, "bad_request", "no such file id")
	})

	_, err := s.master.GetFileInfo(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := f.count("b2_get_file_info"); got != 1 {
		t.Errorf("retried a client error %d times; 4xx will never become 2xx", got)
	}
	if be, ok := AsError(err); !ok || be.Code != "bad_request" {
		t.Errorf("error lost its structure: %v", err)
	}
}

func TestDeleteTreatsMissingAsSuccess(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, false, "")

	f.on("b2_delete_file_version", func(w http.ResponseWriter, _ map[string]any) {
		b2err(w, http.StatusBadRequest, "file_not_present", "already gone")
	})

	// The purge job must survive running twice over the same row.
	if err := s.Delete(context.Background(), "users/1/orig/x.jpg", "file-1"); err != nil {
		t.Errorf("deleting an already-deleted object should succeed, got %v", err)
	}
}

// ---------------------------------------------------------------- verification

func TestVerifyRejectsMismatchedUpload(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, false, "")
	ctx := context.Background()

	f.on("b2_get_file_info", func(w http.ResponseWriter, _ map[string]any) {
		writeJSON(w, http.StatusOK, FileInfo{FileID: "f1", ContentLength: 4 << 30, ContentSha1: "aaa"})
	})

	// The whole point of §4.2 step 2: a client that declares 1 KB and stores
	// 4 GB must not get past the quota.
	if _, err := s.Verify(ctx, "f1", 1024, "aaa"); !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("want ErrSizeMismatch, got %v", err)
	}
	if _, err := s.Verify(ctx, "f1", 4<<30, "bbb"); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("want ErrDigestMismatch, got %v", err)
	}
	if _, err := s.Verify(ctx, "f1", 4<<30, "AAA"); err != nil {
		t.Errorf("digest comparison should be case-insensitive hex, got %v", err)
	}
}

func TestVerifyAcceptsLargeFileDigest(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, false, "")

	f.on("b2_get_file_info", func(w http.ResponseWriter, _ map[string]any) {
		writeJSON(w, http.StatusOK, FileInfo{
			FileID:        "f1",
			ContentLength: 200 << 20,
			ContentSha1:   "none",
			Info:          map[string]string{"large_file_sha1": "abc"},
		})
	})

	if _, err := s.Verify(context.Background(), "f1", 200<<20, "abc"); err != nil {
		t.Errorf("large-file verification failed: %v", err)
	}
}

// ---------------------------------------------------------------- download tokens

func TestPrefixTokenMintedOncePerUser(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, false, "https://cdn.faidz.fun/f")
	ctx := context.Background()

	// A gallery page asks for 50 thumbnails plus 50 originals.
	for i := 0; i < 100; i++ {
		if _, err := s.DownloadURL(ctx, 1, ThumbName(1, "key")); err != nil {
			t.Fatalf("DownloadURL: %v", err)
		}
	}
	if got := f.count("b2_get_download_authorization"); got != 1 {
		t.Fatalf("minted %d download authorizations for one page load; the free tier is 2,500/day (spec §2.4)", got)
	}

	// A different user is a different prefix, so it needs its own token.
	if _, err := s.DownloadURL(ctx, 2, ThumbName(2, "key")); err != nil {
		t.Fatal(err)
	}
	if got := f.count("b2_get_download_authorization"); got != 2 {
		t.Errorf("want a separate token per user prefix, got %d total", got)
	}
}

func TestPrefixTokenSurvivesRestart(t *testing.T) {
	f := newFakeB2(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	first := testServiceWithStore(t, f, st, false, "https://cdn.faidz.fun/f")
	if _, err := first.DownloadURL(ctx, 1, ThumbName(1, "key")); err != nil {
		t.Fatal(err)
	}

	// A fresh Service has an empty in-memory cache and must read the token back
	// from SQLite rather than spending another Class C transaction. A deploy
	// that restarts a few times would otherwise burn the free tier.
	second := testServiceWithStore(t, f, st, false, "https://cdn.faidz.fun/f")
	if _, err := second.DownloadURL(ctx, 1, ThumbName(1, "key")); err != nil {
		t.Fatal(err)
	}

	if got := f.count("b2_get_download_authorization"); got != 1 {
		t.Errorf("restart re-minted the token (%d calls); the b2_tokens cache is not being read", got)
	}
}

func TestDownloadURLShape(t *testing.T) {
	f := newFakeB2(t)
	ctx := context.Background()

	withCDN := testService(t, f, false, "https://cdn.faidz.fun/f")
	got, err := withCDN.DownloadURL(ctx, 1, "users/1/thumb/abc def.webp")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://cdn.faidz.fun/f/users/1/thumb/abc%20def.webp?Authorization=dl-token"
	if got != want {
		t.Errorf("CDN URL\n got %q\nwant %q", got, want)
	}

	direct := testService(t, f, false, "")
	got, err = direct.DownloadURL(ctx, 1, "users/1/thumb/abc.webp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/file/faidz-cloud/users/1/thumb/abc.webp?Authorization=") {
		t.Errorf("direct B2 URL has the wrong shape: %q", got)
	}
}

func TestSignedURLIsPerObject(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, false, "https://cdn.faidz.fun/f")

	var gotPrefix string
	var gotSeconds float64
	f.on("b2_get_download_authorization", func(w http.ResponseWriter, body map[string]any) {
		gotPrefix, _ = body["fileNamePrefix"].(string)
		gotSeconds, _ = body["validDurationInSeconds"].(float64)
		writeJSON(w, http.StatusOK, map[string]string{"authorizationToken": "short-token"})
	})

	name := "users/1/orig/abc.jpg"
	if _, err := s.SignedURL(context.Background(), name, 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	// Share links must be scoped to the single object and expire quickly, or
	// revoking a share would not take effect until the prefix token did.
	if gotPrefix != name {
		t.Errorf("share token scoped to %q, want the single object %q", gotPrefix, name)
	}
	if gotSeconds != 600 {
		t.Errorf("share token valid for %v seconds, want 600", gotSeconds)
	}
}

// ---------------------------------------------------------------- key scoping

func TestBrowserCredentialsRequireRestrictedKey(t *testing.T) {
	f := newFakeB2(t)
	ctx := context.Background()

	// This is the security boundary that makes decision D1 acceptable. Without
	// a restricted key the request must fail, not silently fall back to the
	// master key and hand a browser full bucket access.
	noKey := testService(t, f, false, "")
	if _, err := noKey.UploadCredentials(ctx, "users/1/orig/x.jpg"); !errors.Is(err, ErrNoUploadKey) {
		t.Errorf("want ErrNoUploadKey, got %v", err)
	}

	withKey := testService(t, f, true, "")
	if _, err := withKey.UploadCredentials(ctx, "users/1/orig/x.jpg"); err != nil {
		t.Errorf("with a restricted key configured, minting should succeed: %v", err)
	}

	// The escape hatch exists for local development and must be explicit.
	optIn := testService(t, f, false, "")
	optIn.allowMasterUpload = true
	if _, err := optIn.UploadCredentials(ctx, "users/1/orig/x.jpg"); err != nil {
		t.Errorf("explicit opt-in should be allowed: %v", err)
	}
}

func TestStatusFlagsOverPrivilegedUploadKey(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, true, "")

	if st := s.Status(context.Background()); st.State != "ok" || st.UploadKeyScope != "users/1/" {
		t.Errorf("healthy config reported as %+v", st)
	}

	// An upload key that can read is a silent security downgrade, so health
	// must call it out rather than report ok.
	f2 := newFakeB2(t)
	bad := testService(t, f2, true, "")
	bad.upload = NewClient("upload", "master-key", "secret") // full-capability key
	bad.upload.authBase = f2.srv.URL

	st := bad.Status(context.Background())
	if st.State != "error" || !strings.Contains(st.Detail, "readFiles") {
		t.Errorf("over-privileged upload key not flagged: %+v", st)
	}
}

func TestPartCredentialsOnePerWorker(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, true, "")

	targets, err := s.PartCredentials(context.Background(), "large-1", 4)
	if err != nil {
		t.Fatal(err)
	}
	// A B2 upload URL serves one upload at a time, so four concurrent part
	// workers need four distinct URLs.
	if len(targets) != 4 {
		t.Errorf("got %d part targets, want 4", len(targets))
	}
	if got := f.count("b2_get_upload_part_url"); got != 4 {
		t.Errorf("b2_get_upload_part_url called %d times, want 4", got)
	}
}

func TestFailedAuthorizationIsNegativelyCached(t *testing.T) {
	f := newFakeB2(t)
	s := testService(t, f, false, "")
	ctx := context.Background()

	// Point the client at a host that always rejects the credentials.
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b2err(w, http.StatusUnauthorized, "bad_auth_token", "wrong key")
	}))
	defer rejecting.Close()
	s.master.authBase = rejecting.URL

	// Twenty health checks in quick succession must not become twenty round
	// trips to B2; a monitor polling a misconfigured server would otherwise
	// hammer the authorize endpoint into rate limiting.
	var hits int
	rejecting.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b2err(w, http.StatusUnauthorized, "bad_auth_token", "wrong key")
	})

	for i := 0; i < 20; i++ {
		if st := s.Status(ctx); st.State != "error" {
			t.Fatalf("call %d: want error state, got %+v", i, st)
		}
	}
	if hits != 1 {
		t.Errorf("authorize attempted %d times for 20 health checks, want 1", hits)
	}

	// An expired token is a different situation and must still re-authorize.
	s.master.invalidate()
	if st := s.Status(ctx); st.State != "error" {
		t.Fatalf("unexpected state after invalidate: %+v", st)
	}
	if hits != 2 {
		t.Errorf("invalidate() did not clear the negative cache: %d attempts", hits)
	}
}
