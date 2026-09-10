// Package httpapi wires the HTTP surface described in spec §4.
//
// Step 1 covers auth and health only. File, folder and share routes arrive in
// Step 3; their absence is why the router is small rather than because the
// spec's API is.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"cloudapp/internal/b2"
	"cloudapp/internal/config"
	"cloudapp/internal/store"
)

type Server struct {
	Config  *config.Config
	Store   *store.Store
	B2      *b2.Service
	Log     *slog.Logger
	Version string

	started time.Time
}

func New(cfg *config.Config, st *store.Store, storage *b2.Service, log *slog.Logger, version string) *Server {
	return &Server{Config: cfg, Store: st, B2: storage, Log: log, Version: version, started: time.Now()}
}

func (s *Server) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.RedirectTrailingSlash = false

	// Behind Nginx, ClientIP must come from X-Forwarded-For — otherwise every
	// request looks like 127.0.0.1 and the rate limiter becomes one shared
	// bucket for the whole internet.
	if s.Config.TrustedProxy {
		r.SetTrustedProxies([]string{"127.0.0.1", "::1"})
	} else {
		r.SetTrustedProxies(nil)
	}

	r.Use(recovery(s.Log), requestLog(s.Log), securityHeaders())

	loginLimit := newLimiter(5, time.Minute)    // spec §8
	uploadLimit := newLimiter(120, time.Minute) // spec §8: /api/files/init
	writeLimit := newLimiter(240, time.Minute)  // generous; catches runaway clients, not people

	api := r.Group("/api")
	api.Use(s.requireSameOrigin())
	{
		api.GET("/health", s.handleHealth)

		api.POST("/auth/login", s.rateLimit(loginLimit), s.handleLogin)

		authed := api.Group("")
		authed.Use(s.requireAuth())
		{
			authed.GET("/auth/me", s.handleMe)
			authed.POST("/auth/logout", s.handleLogout)
			authed.POST("/auth/password", s.rateLimit(writeLimit), s.handleChangePassword)

			// Upload is three-phase because file bytes bypass this server
			// entirely (decision D1). Spec §4.2.
			authed.POST("/files/init", s.rateLimit(uploadLimit), s.handleFileInit)
			authed.POST("/files/init-parts", s.rateLimit(uploadLimit), s.handleFileInitParts)
			authed.POST("/files/:id/complete", s.handleFileComplete)
			authed.POST("/files/:id/abort", s.handleFileAbort)

			authed.GET("/files", s.handleFileList)
			authed.GET("/files/:id", s.handleFileGet)
			authed.PATCH("/files/:id", s.rateLimit(writeLimit), s.handleFilePatch)
			authed.DELETE("/files/:id", s.rateLimit(writeLimit), s.handleFileDelete)
			authed.POST("/files/:id/restore", s.rateLimit(writeLimit), s.handleFileRestore)
			authed.POST("/files/bulk-delete", s.rateLimit(writeLimit), s.handleFileBulkDelete)

			authed.GET("/folders", s.handleFolderList)
			authed.POST("/folders", s.rateLimit(writeLimit), s.handleFolderCreate)
			authed.PATCH("/folders/:id", s.rateLimit(writeLimit), s.handleFolderRename)
			authed.DELETE("/folders/:id", s.rateLimit(writeLimit), s.handleFolderDelete)

			authed.GET("/storage/usage", s.handleStorageUsage)

			authed.POST("/shares", s.rateLimit(writeLimit), s.handleShareCreate)
			authed.GET("/shares", s.handleShareList)
			authed.DELETE("/shares/:id", s.rateLimit(writeLimit), s.handleShareRevoke)

			authed.GET("/sessions", s.handleSessionList)
			authed.DELETE("/sessions/:id", s.rateLimit(writeLimit), s.handleSessionRevoke)
			authed.POST("/sessions/revoke-others", s.rateLimit(writeLimit), s.handleSessionRevokeOthers)
		}
	}

	// Public share links. Outside /api because they are opened by people who
	// have no session and no idea this is an API — and rate limited because the
	// token in the path is the entire credential.
	shareLimit := newLimiter(60, time.Minute) // spec §8
	r.GET("/s/:token", s.rateLimit(shareLimit), s.handlePublicShare)

	r.NoRoute(func(c *gin.Context) {
		fail(c, http.StatusNotFound, "not_found", "No such endpoint.")
	})

	return r
}

// Run serves until ctx is cancelled, then drains in-flight requests.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Config.Addr,
		Handler:           s.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: file bytes never pass through here (decision D1), so
		// there is no long-lived response to accommodate, but a share redirect
		// should still never be cut off mid-flight.
		IdleTimeout: 60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("listening", "addr", s.Config.Addr, "version", s.Version)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.Log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
