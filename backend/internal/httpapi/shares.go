package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"cloudapp/internal/b2"
	"cloudapp/internal/store"
)

// shareURLTTL is how long the storage URL a share redirects to stays valid.
//
// Short on purpose. A recipient can save whatever URL they are handed, so
// revoking a share must not have to wait out a seven-day token — which is why
// share downloads redirect through this server instead of being given a
// long-lived storage URL directly (spec §4.5).
const shareURLTTL = 10 * time.Minute

type createShareRequest struct {
	FileID        int64 `json:"file_id" binding:"required"`
	ExpiresInDays int   `json:"expires_in_days"`
}

func (s *Server) handleShareCreate(c *gin.Context) {
	var req createShareRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "Pick a file to share.")
		return
	}

	user := currentUser(c)
	share, err := s.Store.CreateShare(c.Request.Context(), user.ID, req.FileID, req.ExpiresInDays)
	if errors.Is(err, store.ErrNoFile) {
		fail(c, http.StatusNotFound, "not_found", "No such file.")
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "at most") {
			fail(c, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.Log.Error("create share failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not create the link.")
		return
	}

	s.Log.Info("share created", "file_id", req.FileID, "expires_at", share.ExpiresAt)
	c.JSON(http.StatusCreated, s.sharePayload(share))
}

func (s *Server) handleShareList(c *gin.Context) {
	user := currentUser(c)
	shares, err := s.Store.ListShares(c.Request.Context(), user.ID)
	if err != nil {
		s.Log.Error("list shares failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not load your links.")
		return
	}

	items := make([]gin.H, 0, len(shares))
	for _, share := range shares {
		items = append(items, s.sharePayload(share))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (s *Server) handleShareRevoke(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	user := currentUser(c)
	if err := s.Store.RevokeShare(c.Request.Context(), user.ID, id); err != nil {
		if errors.Is(err, store.ErrNoShare) {
			fail(c, http.StatusNotFound, "not_found", "No such link, or it was already revoked.")
			return
		}
		s.Log.Error("revoke share failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not revoke the link.")
		return
	}
	s.Log.Info("share revoked", "share_id", id)
	c.JSON(http.StatusOK, gin.H{"revoked": true})
}

// handlePublicShare is the only unauthenticated route that serves content.
//
// It answers with a redirect, never with bytes: the file still comes straight
// from storage, and this server only decides whether it may (decision D1).
func (s *Server) handlePublicShare(c *gin.Context) {
	token := c.Param("token")
	ctx := c.Request.Context()

	share, file, err := s.Store.ResolveShare(ctx, token)
	if err != nil {
		// Expired and never-existed are answered the same way. Distinguishing
		// them would confirm to someone guessing tokens that they had found a
		// real one.
		if errors.Is(err, store.ErrNoShare) || errors.Is(err, store.ErrShareExpired) ||
			errors.Is(err, store.ErrNoFile) {
			c.String(http.StatusNotFound, "This link is not valid. It may have expired or been revoked.")
			return
		}
		s.Log.Error("resolve share failed", "err", err)
		c.String(http.StatusInternalServerError, "Something went wrong.")
		return
	}

	name := b2.OrigName(file.UserID, file.ObjectKey, file.Ext)
	url, err := s.B2.SignedURL(ctx, name, shareURLTTL)
	if err != nil {
		s.Log.Error("sign share url failed", "err", err)
		c.String(http.StatusBadGateway, "Storage is not responding. Try again shortly.")
		return
	}

	s.Store.TouchShare(ctx, share.ID)

	// Never cached: the URL behind it expires in minutes, and a cached redirect
	// would keep working after the share was revoked.
	c.Header("Cache-Control", "no-store, private")
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusFound, url)
}

func (s *Server) sharePayload(share *store.Share) gin.H {
	payload := gin.H{
		"id":         share.ID,
		"file_id":    share.FileID,
		"filename":   share.Filename,
		"kind":       share.Kind,
		"url":        s.Config.PublicOrigin() + "/s/" + share.Token,
		"expires_at": share.ExpiresAt,
		"created_at": share.CreatedAt,
		"view_count": share.ViewCount,
		"active":     share.Active(time.Now()),
	}
	if share.RevokedAt != nil {
		payload["revoked_at"] = *share.RevokedAt
	}
	return payload
}
