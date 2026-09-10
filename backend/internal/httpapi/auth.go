package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"cloudapp/internal/store"
)

// The session cookie is httpOnly, so no JavaScript ever reads or writes it.
// That removes token theft via XSS as an entire class of attack (spec §8), and
// is why the frontend has no token handling code at all.
func (s *Server) setSessionCookie(c *gin.Context, token string, expires time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   s.Config.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.Config.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (s *Server) handleLogin(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "Enter a username and password.")
		return
	}

	user, err := s.Store.Authenticate(c.Request.Context(), req.Username, req.Password)
	if errors.Is(err, store.ErrBadCredential) {
		// Deliberately does not say which half was wrong.
		fail(c, http.StatusUnauthorized, "invalid_credentials", "That username and password do not match.")
		return
	}
	if err != nil {
		s.Log.Error("authenticate failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Something went wrong. Try again.")
		return
	}

	token, expires, err := s.Store.NewSession(
		c.Request.Context(), user.ID, s.Config.SessionTTL, c.GetHeader("User-Agent"))
	if err != nil {
		s.Log.Error("create session failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Something went wrong. Try again.")
		return
	}

	s.setSessionCookie(c, token, expires)
	s.Log.Info("signed in", "user", user.Username)
	c.JSON(http.StatusOK, userPayload(user))
}

func (s *Server) handleLogout(c *gin.Context) {
	if token, ok := c.Get(ctxTokenKey); ok {
		if err := s.Store.RevokeSession(c.Request.Context(), token.(string)); err != nil {
			s.Log.Error("revoke session failed", "err", err)
		}
	}
	s.clearSessionCookie(c)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleMe(c *gin.Context) {
	c.JSON(http.StatusOK, userPayload(currentUser(c)))
}

type changePasswordRequest struct {
	Current string `json:"current_password" binding:"required"`
	New     string `json:"new_password" binding:"required"`
}

func (s *Server) handleChangePassword(c *gin.Context) {
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "Enter your current and new password.")
		return
	}

	user := currentUser(c)
	ctx := c.Request.Context()

	if _, err := s.Store.Authenticate(ctx, user.Username, req.Current); err != nil {
		fail(c, http.StatusForbidden, "invalid_credentials", "Your current password is not correct.")
		return
	}
	if err := s.Store.SetPassword(ctx, user.ID, req.New); err != nil {
		fail(c, http.StatusBadRequest, "weak_password", err.Error())
		return
	}

	// A password change that leaves existing sessions alive does not lock
	// anyone out, which is usually the entire reason for changing it.
	if _, err := s.Store.RevokeAllSessions(ctx, user.ID); err != nil {
		s.Log.Error("revoke sessions after password change failed", "err", err)
	}

	token, expires, err := s.Store.NewSession(ctx, user.ID, s.Config.SessionTTL, c.GetHeader("User-Agent"))
	if err != nil {
		s.clearSessionCookie(c)
		c.JSON(http.StatusOK, gin.H{"ok": true, "signed_out": true})
		return
	}
	s.setSessionCookie(c, token, expires)
	c.JSON(http.StatusOK, gin.H{"ok": true, "other_sessions_revoked": true})
}

func userPayload(u *store.User) gin.H {
	if u == nil {
		return gin.H{}
	}
	payload := gin.H{
		"username":      u.Username,
		"storage_used":  u.StorageUsed,
		"storage_quota": u.StorageQuota,
	}
	if u.LastLoginAt != nil {
		payload["last_login_at"] = u.LastLoginAt.Unix()
	}
	return payload
}
