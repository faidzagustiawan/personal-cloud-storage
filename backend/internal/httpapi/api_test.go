package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudapp/internal/b2"
	"cloudapp/internal/config"
	"cloudapp/internal/store"
)

// End-to-end over the real router, with B2 replaced by a fake. This is what
// catches wiring mistakes — a route registered on the wrong group, a payload
// field that never made it into the JSON — which unit tests on the layers
// underneath cannot see.

const testOrigin = "https://cloud.faidz.fun"

type harness struct {
	t      *testing.T
	server *Server
	router http.Handler
	cookie string
	b2Hits map[string]int

	fakeURL string
	// What the fake B2 reports as actually stored, so a test can make the
	// client's declaration and reality disagree.
	storedSize int64
	storedSha1 string

	// Per-endpoint overrides for the fake B2, and the server's log output.
	overrides map[string]func(http.ResponseWriter, map[string]any)
	logs      *bytes.Buffer
}

// onB2 replaces the fake's response for one endpoint.
func (h *harness) onB2(endpoint string, fn func(http.ResponseWriter, map[string]any)) {
	h.overrides[endpoint] = fn
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		t:         t,
		b2Hits:    map[string]int{},
		overrides: map[string]func(http.ResponseWriter, map[string]any){},
		logs:      &bytes.Buffer{},
	}

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		h.b2Hits[endpoint]++

		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			json.Unmarshal(raw, &body)
		}

		if override := h.overrides[endpoint]; override != nil {
			w.Header().Set("Content-Type", "application/json")
			override(w, body)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch endpoint {
		case "b2_authorize_account":
			keyID, _, _ := r.BasicAuth()
			prefix := "users/1/"
			allowed := map[string]any{
				"capabilities": []string{"listFiles", "readFiles", "writeFiles", "deleteFiles", "shareFiles"},
				"bucketId":     "bucket-1",
			}
			if keyID == "upload-key" {
				allowed = map[string]any{"capabilities": []string{"writeFiles"}, "bucketId": "bucket-1", "namePrefix": prefix}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"accountId": "acct", "authorizationToken": "auth", "apiUrl": h.b2URL(),
				"downloadUrl": h.b2URL(), "recommendedPartSize": 100 << 20,
				"absoluteMinimumPartSize": 5 << 20, "allowed": allowed,
			})
		case "b2_get_upload_url", "b2_get_upload_part_url":
			json.NewEncoder(w).Encode(map[string]any{
				"uploadUrl": h.b2URL() + "/upload", "authorizationToken": "up", "fileId": "large-1",
			})
		case "b2_start_large_file":
			json.NewEncoder(w).Encode(map[string]any{"fileId": "large-1"})
		case "b2_finish_large_file":
			json.NewEncoder(w).Encode(map[string]any{"fileId": "b2-large", "contentLength": body["_"], "contentSha1": "none"})
		case "b2_get_file_info":
			// Echo back whatever the harness was told to pretend is stored.
			json.NewEncoder(w).Encode(map[string]any{
				"fileId": body["fileId"], "contentLength": h.storedSize, "contentSha1": h.storedSha1,
			})
		case "b2_get_download_authorization":
			json.NewEncoder(w).Encode(map[string]any{"authorizationToken": "dl"})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(fake.Close)
	h.fakeURL = fake.URL

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Addr: "127.0.0.1:0", Origins: []string{testOrigin},
		SessionTTL: 24 * time.Hour, DefaultQuota: 1 << 30,
		B2KeyID: "master-key", B2AppKey: "s", B2UploadKeyID: "upload-key", B2UploadKey: "s",
		B2BucketID: "bucket-1", B2BucketName: "faidz-cloud",
		CDNBase: "https://cdn.faidz.fun/f", B2APIBase: fake.URL,
	}

	// Logs go to a buffer so a test can assert what does and does not reach them.
	log := slog.New(slog.NewTextHandler(h.logs, nil))
	storage := b2.NewService(cfg, st, log, false)
	h.server = New(cfg, st, storage, log, "test")
	h.router = h.server.Router()
	return h
}

func (h *harness) b2URL() string { return h.fakeURL }

func (h *harness) do(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.doWithAgent(method, path, body, "harness/1.0")
}

func (h *harness) doWithAgent(method, path string, body any, userAgent string) *httptest.ResponseRecorder {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("User-Agent", userAgent)
	if h.cookie != "" {
		req.Header.Set("Cookie", h.cookie)
	}

	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	if sc := rec.Header().Get("Set-Cookie"); sc != "" {
		h.cookie = strings.SplitN(sc, ";", 2)[0]
	}
	return rec
}

// login signs in, creating the account the first time. Tests switch back and
// forth between two users, so an existing account is not an error here.
func (h *harness) login(username string) {
	h.t.Helper()
	_, err := h.server.Store.CreateUser(h.t.Context(), username, "a-long-enough-password", 1<<30)
	if err != nil && !errors.Is(err, store.ErrUserExists) {
		h.t.Fatalf("create user: %v", err)
	}
	rec := h.do(http.MethodPost, "/api/auth/login",
		map[string]string{"username": username, "password": "a-long-enough-password"})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// uploadFile drives the full three-phase flow the browser performs.
func (h *harness) uploadFile(name string, size int64, mime string) map[string]any {
	h.t.Helper()

	rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
		"filename": name, "size": size, "mime_type": mime, "sha1": "sha-" + name,
	})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("init %s: %d %s", name, rec.Code, rec.Body.String())
	}
	init := decode(h.t, rec)

	// The browser would now put the bytes in B2. Tell the fake what landed.
	h.storedSize, h.storedSha1 = size, "sha-"+name

	fileID := int64(init["file_id"].(float64))
	rec = h.do(http.MethodPost, "/api/files/"+itoa(fileID)+"/complete", map[string]any{
		"b2_file_id": "b2-" + name, "sha1": "sha-" + name,
		"width": 4032, "height": 3024, "thumb_ok": true,
		"thumb_tier": "native", "thumb_format": "image/webp",
	})
	if rec.Code != http.StatusOK {
		h.t.Fatalf("complete %s: %d %s", name, rec.Code, rec.Body.String())
	}
	return decode(h.t, rec)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ---------------------------------------------------------------- tests

func TestUploadRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	file := h.uploadFile("IMG_0042.HEIC", 2<<20, "image/heic")

	if file["filename"] != "IMG_0042.HEIC" {
		t.Errorf("filename = %v", file["filename"])
	}
	if file["thumb_status"] != "ready" {
		t.Errorf("thumb_status = %v", file["thumb_status"])
	}

	// The gallery needs a ready-to-use URL, composed server-side from the
	// cached prefix token (spec §4.3).
	orig, _ := file["orig_url"].(string)
	if !strings.HasPrefix(orig, "https://cdn.faidz.fun/f/users/1/orig/") || !strings.Contains(orig, "?Authorization=dl") {
		t.Errorf("orig_url has the wrong shape: %q", orig)
	}
	thumb, _ := file["thumb_url"].(string)
	if !strings.Contains(thumb, "/users/1/thumb/") || !strings.HasSuffix(thumb, "?Authorization=dl") {
		t.Errorf("thumb_url has the wrong shape: %q", thumb)
	}
	// The extension came from the client's filename and must be lowercased.
	if !strings.HasSuffix(strings.SplitN(orig, "?", 2)[0], ".heic") {
		t.Errorf("extension was not normalised: %q", orig)
	}

	rec := h.do(http.MethodGet, "/api/files", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	items := decode(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("gallery has %d items, want 1", len(items))
	}

	rec = h.do(http.MethodGet, "/api/storage/usage", nil)
	usage := decode(t, rec)
	if int64(usage["used_bytes"].(float64)) != 2<<20 {
		t.Errorf("used_bytes = %v", usage["used_bytes"])
	}
}

func TestCompleteRejectsUnverifiableUpload(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
		"filename": "small.jpg", "size": 1024, "mime_type": "image/jpeg", "sha1": "sha-a",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init: %d %s", rec.Code, rec.Body.String())
	}
	fileID := int64(decode(t, rec)["file_id"].(float64))

	// The client declared 1 KB but 4 GB actually landed in B2. This is the
	// exact attack that verification exists to stop (spec §4.2).
	h.storedSize, h.storedSha1 = 4<<30, "sha-a"

	rec = h.do(http.MethodPost, "/api/files/"+itoa(fileID)+"/complete", map[string]any{
		"b2_file_id": "b2-a", "sha1": "sha-a", "thumb_ok": true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if code := decode(t, rec)["error"].(map[string]any)["code"]; code != "verification_failed" {
		t.Errorf("error code = %v", code)
	}

	// Nothing may be left behind: no row, no quota charged.
	usage := decode(t, h.do(http.MethodGet, "/api/storage/usage", nil))
	if usage["used_bytes"].(float64) != 0 {
		t.Errorf("quota was charged for a rejected upload: %v", usage["used_bytes"])
	}
	if h.b2Hits["b2_delete_file_version"] == 0 {
		t.Error("the unverifiable object was left in the bucket")
	}
}

func TestRejectsUnsupportedType(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
		"filename": "notes.pdf", "size": 1024, "mime_type": "application/pdf",
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("want 415 for a PDF, got %d", rec.Code)
	}
}

func TestQuotaRejectedAtInit(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	// The quota is 1 GB and the per-file ceiling is 500 MB, so three of these
	// fit and the fourth must not.
	for i := 0; i < 2; i++ {
		rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
			"filename": "clip.mov", "size": 400 << 20, "mime_type": "video/quicktime",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("reservation %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
		"filename": "clip.mov", "size": 400 << 20, "mime_type": "video/quicktime",
	})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d %s", rec.Code, rec.Body.String())
	}
	if code := decode(t, rec)["error"].(map[string]any)["code"]; code != "quota_exceeded" {
		t.Errorf("error code = %v", code)
	}
}

func TestLargeFileTakesMultipartPath(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
		"filename": "big.mov", "size": 200 << 20, "mime_type": "video/quicktime", "sha1": "abc",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init: %d %s", rec.Code, rec.Body.String())
	}
	init := decode(t, rec)

	if init["multipart"] != true {
		t.Error("a 200 MB file did not take the multipart path")
	}
	if init["large_file_id"] != "large-1" {
		t.Errorf("large_file_id = %v", init["large_file_id"])
	}
	if int64(init["part_size"].(float64)) != b2.PartSize {
		t.Errorf("part_size = %v, want %d", init["part_size"], b2.PartSize)
	}

	// One upload URL per concurrent worker: a B2 upload URL serves one at a time.
	rec = h.do(http.MethodPost, "/api/files/init-parts", map[string]any{
		"file_id": init["file_id"], "large_file_id": "large-1", "count": 4,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init-parts: %d %s", rec.Code, rec.Body.String())
	}
	urls := decode(t, rec)["urls"].([]any)
	if len(urls) != 4 {
		t.Errorf("got %d part urls, want 4", len(urls))
	}
}

func TestSmallFileSkipsMultipart(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
		"filename": "small.jpg", "size": 5 << 20, "mime_type": "image/jpeg",
	})
	init := decode(t, rec)
	if init["multipart"] != false {
		t.Error("a 5 MB file took the multipart path")
	}
	if init["orig"] == nil {
		t.Error("no single-shot upload credential was issued")
	}
	if h.b2Hits["b2_start_large_file"] != 0 {
		t.Error("b2_start_large_file was called for a small file")
	}
}

func TestDuplicateUploadRejected(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	h.uploadFile("photo.jpg", 1<<20, "image/jpeg")

	rec := h.do(http.MethodPost, "/api/files/init", map[string]any{
		"filename": "photo-copy.jpg", "size": 1 << 20, "mime_type": "image/jpeg", "sha1": "sha-photo.jpg",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 for a duplicate, got %d %s", rec.Code, rec.Body.String())
	}
	if decode(t, rec)["file"] == nil {
		t.Error("the response should point at the file already stored")
	}
}

func TestCrossUserAccessIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.login("alice")
	file := h.uploadFile("private.jpg", 1<<20, "image/jpeg")
	fileID := itoa(int64(file["id"].(float64)))

	// Switch to a second account on the same server.
	h.cookie = ""
	h.login("bob")

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/files/" + fileID},
		{http.MethodDelete, "/api/files/" + fileID},
	} {
		rec := h.do(probe.method, probe.path, nil)
		// 404, never 403: a 403 would confirm the id exists.
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s returned %d, want 404", probe.method, probe.path, rec.Code)
		}
	}

	rec := h.do(http.MethodGet, "/api/files", nil)
	if items := decode(t, rec)["items"].([]any); len(items) != 0 {
		t.Errorf("bob can see %d of alice's files", len(items))
	}
}

func TestTrashAndRestore(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("x.jpg", 3<<20, "image/jpeg")
	id := itoa(int64(file["id"].(float64)))

	if rec := h.do(http.MethodDelete, "/api/files/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}

	if items := decode(t, h.do(http.MethodGet, "/api/files", nil))["items"].([]any); len(items) != 0 {
		t.Error("a trashed file is still in the gallery")
	}
	if items := decode(t, h.do(http.MethodGet, "/api/files?deleted=1", nil))["items"].([]any); len(items) != 1 {
		t.Error("the trashed file is not in the trash view")
	}
	if used := decode(t, h.do(http.MethodGet, "/api/storage/usage", nil))["used_bytes"].(float64); used != 0 {
		t.Errorf("quota not released on delete: %v", used)
	}

	if rec := h.do(http.MethodPost, "/api/files/"+id+"/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	if used := decode(t, h.do(http.MethodGet, "/api/storage/usage", nil))["used_bytes"].(float64); used != 3<<20 {
		t.Errorf("quota not restored: %v", used)
	}
}

func TestFolderLifecycle(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	rec := h.do(http.MethodPost, "/api/folders", map[string]string{"name": "Holiday"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create folder: %d %s", rec.Code, rec.Body.String())
	}
	folderID := int64(decode(t, rec)["id"].(float64))

	// Case-insensitive duplicate: "holiday" beside "Holiday" in the sidebar is
	// a bug report waiting to happen.
	if rec := h.do(http.MethodPost, "/api/folders", map[string]string{"name": "holiday"}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate folder name returned %d, want 409", rec.Code)
	}

	file := h.uploadFile("beach.jpg", 1<<20, "image/jpeg")
	fileID := itoa(int64(file["id"].(float64)))

	if rec := h.do(http.MethodPatch, "/api/files/"+fileID, map[string]any{"folder_id": folderID}); rec.Code != http.StatusOK {
		t.Fatalf("move into folder: %d %s", rec.Code, rec.Body.String())
	}

	// Deleting a folder full of photos is not obviously the same request as
	// deleting the folder.
	if rec := h.do(http.MethodDelete, "/api/folders/"+itoa(folderID), nil); rec.Code != http.StatusConflict {
		t.Errorf("non-empty folder delete returned %d, want 409", rec.Code)
	}
	if rec := h.do(http.MethodDelete, "/api/folders/"+itoa(folderID)+"?move_to=root", nil); rec.Code != http.StatusOK {
		t.Errorf("move_to=root delete returned %d", rec.Code)
	}
	if items := decode(t, h.do(http.MethodGet, "/api/files", nil))["items"].([]any); len(items) != 1 {
		t.Error("the file did not survive its folder being deleted")
	}
}

func TestPaginationCursor(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	for i := 0; i < 5; i++ {
		h.uploadFile("p"+itoa(int64(i))+".jpg", 1<<20, "image/jpeg")
	}

	first := decode(t, h.do(http.MethodGet, "/api/files?limit=2", nil))
	if len(first["items"].([]any)) != 2 {
		t.Fatalf("first page has %d items", len(first["items"].([]any)))
	}
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("no cursor for a second page")
	}

	second := decode(t, h.do(http.MethodGet, "/api/files?limit=2&cursor="+cursor, nil))
	if len(second["items"].([]any)) != 2 {
		t.Errorf("second page has %d items", len(second["items"].([]any)))
	}

	firstIDs := map[float64]bool{}
	for _, it := range first["items"].([]any) {
		firstIDs[it.(map[string]any)["id"].(float64)] = true
	}
	for _, it := range second["items"].([]any) {
		if firstIDs[it.(map[string]any)["id"].(float64)] {
			t.Error("the second page repeated a row from the first")
		}
	}

	if rec := h.do(http.MethodGet, "/api/files?cursor=garbage!!", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed cursor returned %d, want 400", rec.Code)
	}
}

func TestFileRoutesRequireAuth(t *testing.T) {
	h := newHarness(t)

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/files"},
		{http.MethodPost, "/api/files/init"},
		{http.MethodGet, "/api/folders"},
		{http.MethodGet, "/api/storage/usage"},
	} {
		rec := h.do(probe.method, probe.path, map[string]any{})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s returned %d without a session, want 401", probe.method, probe.path, rec.Code)
		}
	}
}

func TestOnePrefixTokenServesAWholePage(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	for i := 0; i < 10; i++ {
		h.uploadFile("p"+itoa(int64(i))+".jpg", 1<<20, "image/jpeg")
	}

	before := h.b2Hits["b2_get_download_authorization"]
	h.do(http.MethodGet, "/api/files", nil)
	after := h.b2Hits["b2_get_download_authorization"]

	// Twenty URLs on the page, zero new Class C transactions: the token was
	// minted once and cached (spec §2.4).
	if after != before {
		t.Errorf("listing spent %d download authorizations; the cache is not working", after-before)
	}
}
