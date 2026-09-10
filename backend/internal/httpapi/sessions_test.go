package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// Opaque database-backed sessions (decision D3) exist so that a forgotten or
// stolen one can be cut off without changing the password. These tests are
// what make that claim true rather than aspirational.

// secondSession signs the same user in again from another "device" and returns
// that session's cookie, leaving h.cookie pointing at it.
func (h *harness) secondSession(username, userAgent string) string {
	h.t.Helper()
	previous := h.cookie
	h.cookie = ""

	rec := h.doWithAgent(http.MethodPost, "/api/auth/login",
		map[string]string{"username": username, "password": "a-long-enough-password"}, userAgent)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("second login: %d %s", rec.Code, rec.Body.String())
	}
	second := h.cookie
	h.cookie = previous
	return second
}

func TestSessionsAreListedWithTheCurrentOneMarked(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	h.secondSession("faidz", "Mozilla/5.0 (iPhone)")

	rec := h.do(http.MethodGet, "/api/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list sessions: %d %s", rec.Code, rec.Body.String())
	}
	items := decode(t, rec)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(items))
	}

	current := 0
	for _, item := range items {
		row := item.(map[string]any)
		if row["current"] == true {
			current++
		}
		// The id is the token's SHA-256, not the token: exposing it cannot let
		// anyone authenticate, because the preimage is what the cookie holds.
		if id, _ := row["id"].(string); len(id) != 64 {
			t.Errorf("session id is not a sha256 digest: %q", id)
		}
	}
	if current != 1 {
		t.Errorf("%d sessions claim to be the current one, want exactly 1", current)
	}
}

func TestRevokingAnotherSessionEndsItButNotThisOne(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	phone := h.secondSession("faidz", "Mozilla/5.0 (iPhone)")

	// Find the one that is not us.
	items := decode(t, h.do(http.MethodGet, "/api/sessions", nil))["items"].([]any)
	var other string
	for _, item := range items {
		row := item.(map[string]any)
		if row["current"] != true {
			other, _ = row["id"].(string)
		}
	}
	if other == "" {
		t.Fatal("no other session found")
	}

	rec := h.do(http.MethodDelete, "/api/sessions/"+other, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if decode(t, rec)["signed_out"] != false {
		t.Error("revoking another device signed us out")
	}

	// Ours still works.
	if rec := h.do(http.MethodGet, "/api/auth/me", nil); rec.Code != http.StatusOK {
		t.Errorf("current session stopped working: %d", rec.Code)
	}

	// The phone's does not.
	saved := h.cookie
	h.cookie = phone
	if rec := h.do(http.MethodGet, "/api/auth/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked session still authenticates: %d", rec.Code)
	}
	h.cookie = saved
}

func TestRevokingOwnSessionClearsTheCookie(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")

	items := decode(t, h.do(http.MethodGet, "/api/sessions", nil))["items"].([]any)
	own, _ := items[0].(map[string]any)["id"].(string)

	rec := h.do(http.MethodDelete, "/api/sessions/"+own, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke own: %d %s", rec.Code, rec.Body.String())
	}
	if decode(t, rec)["signed_out"] != true {
		t.Error("revoking our own session did not report a sign-out")
	}

	// Leaving the cookie in place would mean the browser holds a token the
	// server has already rejected — a 401 on the next click with no explanation.
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "Max-Age=0") &&
		!strings.Contains(sc, "1970") {
		t.Errorf("session cookie was not cleared: %q", sc)
	}
}

func TestRevokeOthersKeepsOnlyTheCurrentSession(t *testing.T) {
	h := newHarness(t)
	h.login("faidz")
	phone := h.secondSession("faidz", "iPhone")
	laptop := h.secondSession("faidz", "Macintosh")

	rec := h.do(http.MethodPost, "/api/sessions/revoke-others", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke others: %d %s", rec.Code, rec.Body.String())
	}
	if n := decode(t, rec)["revoked"].(float64); n != 2 {
		t.Errorf("revoked %v sessions, want 2", n)
	}

	if rec := h.do(http.MethodGet, "/api/auth/me", nil); rec.Code != http.StatusOK {
		t.Errorf("our own session was revoked too: %d", rec.Code)
	}
	for name, cookie := range map[string]string{"phone": phone, "laptop": laptop} {
		saved := h.cookie
		h.cookie = cookie
		if rec := h.do(http.MethodGet, "/api/auth/me", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s session survived: %d", name, rec.Code)
		}
		h.cookie = saved
	}
}

func TestCannotRevokeAnotherUsersSession(t *testing.T) {
	h := newHarness(t)
	h.login("alice")
	aliceSessions := decode(t, h.do(http.MethodGet, "/api/sessions", nil))["items"].([]any)
	aliceID, _ := aliceSessions[0].(map[string]any)["id"].(string)
	aliceCookie := h.cookie

	h.cookie = ""
	h.login("bob")
	if rec := h.do(http.MethodDelete, "/api/sessions/"+aliceID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("bob revoked alice's session: %d", rec.Code)
	}

	h.cookie = aliceCookie
	if rec := h.do(http.MethodGet, "/api/auth/me", nil); rec.Code != http.StatusOK {
		t.Errorf("alice's session was affected: %d", rec.Code)
	}
}
