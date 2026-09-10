package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// handleHealth implements spec §4.6.
//
// It reports what is actually true rather than a flat "ok". In particular the
// B2 section states whether the restricted upload key is scoped the way §2.3
// requires — a misconfigured key is a silent security downgrade, and this is
// where it becomes visible.
func (s *Server) handleHealth(c *gin.Context) {
	ctx := c.Request.Context()

	dbOK := s.Store.Healthy(ctx) == nil
	b2 := s.B2.Status(ctx)

	status := "ok"
	code := http.StatusOK
	switch {
	case !dbOK:
		status, code = "degraded", http.StatusServiceUnavailable
	case b2.State == "error":
		status, code = "degraded", http.StatusServiceUnavailable
	case b2.State == "unconfigured":
		status = "incomplete"
	}

	users, err := s.Store.CountUsers(ctx)
	if err != nil {
		users = -1
	}

	c.JSON(code, gin.H{
		"status":     status,
		"version":    s.Version,
		"db_ok":      dbOK,
		"b2":         b2,
		"users":      users,
		"uptime_sec": int64(time.Since(s.started).Seconds()),
	})
}
