package httpapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"cloudapp/internal/store"
)

// Sessions are listed so a stolen or forgotten one can be cut off without
// changing the password. Opaque database-backed tokens (decision D3) are what
// make this possible at all — a stateless JWT could not be revoked.

func (s *Server) handleSessionList(c *gin.Context) {
	user := currentUser(c)
	token, _ := c.Get(ctxTokenKey)
	current, _ := token.(string)

	sessions, err := s.Store.ListSessions(c.Request.Context(), user.ID, current)
	if err != nil {
		s.Log.Error("list sessions failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not load your sessions.")
		return
	}

	items := make([]gin.H, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, gin.H{
			"id":         session.ID,
			"created_at": session.CreatedAt,
			"expires_at": session.ExpiresAt,
			"user_agent": session.UserAgent,
			"current":    session.Current,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (s *Server) handleSessionRevoke(c *gin.Context) {
	id := c.Param("id")
	user := currentUser(c)
	ctx := c.Request.Context()

	token, _ := c.Get(ctxTokenKey)
	current, _ := token.(string)

	if err := s.Store.RevokeSessionByID(ctx, user.ID, id); err != nil {
		if errors.Is(err, store.ErrNoSession) {
			fail(c, http.StatusNotFound, "not_found", "No such session, or it already ended.")
			return
		}
		s.Log.Error("revoke session failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not end that session.")
		return
	}

	// Revoking your own session is a sign-out, so the cookie goes too — leaving
	// it behind would mean a browser holding a token the server has already
	// rejected, and a 401 on the next click with no explanation.
	selfSignOut := false
	if sessions, err := s.Store.ListSessions(ctx, user.ID, current); err == nil {
		selfSignOut = true
		for _, session := range sessions {
			if session.Current {
				selfSignOut = false
				break
			}
		}
	}
	if selfSignOut {
		s.clearSessionCookie(c)
	}

	s.Log.Info("session revoked", "user", user.Username, "self", selfSignOut)
	c.JSON(http.StatusOK, gin.H{"revoked": true, "signed_out": selfSignOut})
}

func (s *Server) handleSessionRevokeOthers(c *gin.Context) {
	user := currentUser(c)
	token, _ := c.Get(ctxTokenKey)
	current, _ := token.(string)

	n, err := s.Store.RevokeOtherSessions(c.Request.Context(), user.ID, current)
	if err != nil {
		s.Log.Error("revoke other sessions failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not end the other sessions.")
		return
	}
	s.Log.Info("other sessions revoked", "user", user.Username, "count", n)
	c.JSON(http.StatusOK, gin.H{"revoked": n})
}
