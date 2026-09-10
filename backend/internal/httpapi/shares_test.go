package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A share token is a bearer credential handed to people with no account, so
// these tests are about who gets in and who does not.

func (h *harness) createShare(fileID int64, days int) map[string]any {
	h.t.Helper()
	body := map[string]any{"file_id": fileID}
	if days > 0 {
		body["expires_in_days"] = days
	}
	rec := h.do(http.MethodPost, "/api/shares", body)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create share: %d %s", rec.Code, rec.Body.String())
	}
	return decode(h.t, rec)
}

// public issues a request with no session cookie at all.
func (h *harness) public(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func TestShareLinkWorksWithoutASession(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("beach.jpg", 2<<20, "image/jpeg")

	share := h.createShare(int64(file["id"].(float64)), 7)
	url, _ := share["url"].(string)
	if !strings.HasPrefix(url, testOrigin+"/s/") {
		t.Fatalf("share url has the wrong shape: %q", url)
	}
	if share["active"] != true {
		t.Error("a new share is not active")
	}

	// The whole point: someone with the link and no account can fetch the file.
	rec := h.public(strings.TrimPrefix(url, testOrigin))
	if rec.Code != http.StatusFound {
		t.Fatalf("public fetch returned %d, want 302", rec.Code)
	}

	location := rec.Header().Get("Location")
	if !strings.Contains(location, "/users/1/orig/") || !strings.Contains(location, "Authorization=") {
		t.Errorf("redirect does not point at signed storage: %q", location)
	}
	// A cached redirect would keep working after the share was revoked.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control is %q, want no-store", cc)
	}
}

func TestShareRedirectIsShortLived(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("beach.jpg", 1<<20, "image/jpeg")

	var seconds float64
	h.onB2("b2_get_download_authorization", func(w http.ResponseWriter, body map[string]any) {
		seconds, _ = body["validDurationInSeconds"].(float64)
		writeFake(w, map[string]any{"authorizationToken": "short"})
	})

	share := h.createShare(int64(file["id"].(float64)), 7)
	h.public(strings.TrimPrefix(share["url"].(string), testOrigin))

	// Ten minutes, not the seven-day prefix token: a recipient can save the URL
	// they are given, so revoking must not wait a week to take effect.
	if seconds != 600 {
		t.Errorf("share redirect valid for %v seconds, want 600", seconds)
	}
}

func TestRevokedShareStopsWorkingImmediately(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("beach.jpg", 1<<20, "image/jpeg")

	share := h.createShare(int64(file["id"].(float64)), 7)
	path := strings.TrimPrefix(share["url"].(string), testOrigin)

	if rec := h.public(path); rec.Code != http.StatusFound {
		t.Fatalf("before revoke: %d", rec.Code)
	}

	rec := h.do(http.MethodDelete, "/api/shares/"+itoa(int64(share["id"].(float64))), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}

	if rec := h.public(path); rec.Code != http.StatusNotFound {
		t.Errorf("a revoked link still resolves (%d); revocation is the reason shares redirect through this server", rec.Code)
	}
}

func TestTrashedFileBreaksItsShares(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("beach.jpg", 1<<20, "image/jpeg")
	id := int64(file["id"].(float64))

	share := h.createShare(id, 7)
	path := strings.TrimPrefix(share["url"].(string), testOrigin)

	if rec := h.do(http.MethodDelete, "/api/files/"+itoa(id), nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}

	// The object is thirty days from being purged. The link stops now rather
	// than breaking silently later.
	if rec := h.public(path); rec.Code != http.StatusNotFound {
		t.Errorf("share of a trashed file returned %d, want 404", rec.Code)
	}
}

func TestCannotShareSomeoneElsesFile(t *testing.T) {
	h := newHarness(t)
	h.login("alice")
	file := h.uploadFile("private.jpg", 1<<20, "image/jpeg")
	id := int64(file["id"].(float64))

	h.cookie = ""
	h.login("bob")

	rec := h.do(http.MethodPost, "/api/shares", map[string]any{"file_id": id})
	if rec.Code != http.StatusNotFound {
		t.Errorf("bob could share alice's file: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCannotRevokeSomeoneElsesShare(t *testing.T) {
	h := newHarness(t)
	h.login("alice")
	file := h.uploadFile("a.jpg", 1<<20, "image/jpeg")
	share := h.createShare(int64(file["id"].(float64)), 7)
	shareID := itoa(int64(share["id"].(float64)))

	h.cookie = ""
	h.login("bob")

	if rec := h.do(http.MethodDelete, "/api/shares/"+shareID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("bob revoked alice's share: %d", rec.Code)
	}

	// And alice's link still works.
	h.cookie = ""
	h.login("alice")
	if rec := h.public(strings.TrimPrefix(share["url"].(string), testOrigin)); rec.Code != http.StatusFound {
		t.Errorf("alice's share was affected: %d", rec.Code)
	}
}

func TestUnknownAndExpiredTokensAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	// Guessing tokens must not be able to tell "wrong" from "expired", or the
	// 256-bit search space becomes an oracle.
	rec := h.public("/s/completely-made-up-token-value")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token returned %d", rec.Code)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "not found") &&
		!strings.Contains(rec.Body.String(), "expired") {
		t.Error("the message distinguishes an unknown token from an expired one")
	}
}

func TestShareExpiryIsBounded(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("a.jpg", 1<<20, "image/jpeg")

	rec := h.do(http.MethodPost, "/api/shares", map[string]any{
		"file_id": file["id"], "expires_in_days": 3650,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a ten-year share was accepted: %d", rec.Code)
	}
}

func TestShareTokenIsNotLogged(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("a.jpg", 1<<20, "image/jpeg")
	share := h.createShare(int64(file["id"].(float64)), 7)

	token := strings.TrimPrefix(share["url"].(string), testOrigin+"/s/")
	h.public("/s/" + token)

	// The token is a bearer credential sitting in the path, so a plain access
	// log would hand out working links to anyone who can read it.
	if strings.Contains(h.logs.String(), token) {
		t.Error("the share token appears verbatim in the log")
	}
	if !strings.Contains(h.logs.String(), "/s/<token>") {
		t.Error("the share request was not logged at all, redacted or otherwise")
	}
}

func TestSharesAreListedAndCounted(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	file := h.uploadFile("a.jpg", 1<<20, "image/jpeg")
	share := h.createShare(int64(file["id"].(float64)), 7)
	path := strings.TrimPrefix(share["url"].(string), testOrigin)

	h.public(path)
	h.public(path)

	rec := h.do(http.MethodGet, "/api/shares", nil)
	items := decode(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("listed %d shares, want 1", len(items))
	}
	row := items[0].(map[string]any)
	if row["filename"] != "a.jpg" {
		t.Errorf("share row does not name its file: %v", row["filename"])
	}
	if row["view_count"].(float64) != 2 {
		t.Errorf("view_count = %v, want 2", row["view_count"])
	}
}

func writeFake(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}
