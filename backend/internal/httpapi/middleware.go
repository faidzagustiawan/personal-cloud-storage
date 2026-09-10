package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"cloudapp/internal/store"
)

const (
	sessionCookie = "sid"
	ctxUserKey    = "user"
	ctxTokenKey   = "session_token"
)

// ---------------------------------------------------------------- errors

// fail writes the single error shape the whole API uses (spec §4).
func fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{"code": code, "message": message},
	})
}

// ---------------------------------------------------------------- auth

// requireAuth resolves the session cookie to a user, or rejects with 401.
func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookie)
		if err != nil || token == "" {
			fail(c, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}

		user, err := s.Store.LookupSession(c.Request.Context(), token, s.Config.SessionTTL)
		if errors.Is(err, store.ErrNoSession) || errors.Is(err, store.ErrNoUser) {
			s.clearSessionCookie(c)
			fail(c, http.StatusUnauthorized, "session_expired", "Your session has expired. Sign in again.")
			return
		}
		if err != nil {
			s.Log.Error("session lookup failed", "err", err)
			fail(c, http.StatusInternalServerError, "internal", "Something went wrong. Try again.")
			return
		}

		c.Set(ctxUserKey, user)
		c.Set(ctxTokenKey, token)
		c.Next()
	}
}

func currentUser(c *gin.Context) *store.User {
	v, ok := c.Get(ctxUserKey)
	if !ok {
		return nil
	}
	u, _ := v.(*store.User)
	return u
}

// ---------------------------------------------------------------- CSRF

// requireSameOrigin rejects mutating requests that did not come from a
// configured origin.
//
// SameSite=Lax already blocks cross-site POSTs from a plain form submission,
// but it does not cover every case, and it depends on browser behaviour rather
// than on something this server checks. Since the SPA is same-origin anyway
// (spec §4), an exact Origin match costs nothing and closes the gap.
func (s *Server) requireSameOrigin() gin.HandlerFunc {
	allowed := make(map[string]bool, len(s.Config.Origins))
	for _, o := range s.Config.Origins {
		allowed[o] = true
	}

	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}

		origin := strings.TrimRight(c.GetHeader("Origin"), "/")
		if origin == "" {
			// No Origin header on a same-origin request from an older client:
			// fall back to Referer, which browsers do send.
			if ref := c.GetHeader("Referer"); ref != "" {
				if u, err := url.Parse(ref); err == nil && u.Scheme != "" {
					origin = u.Scheme + "://" + u.Host
				}
			}
		}
		if origin == "" || !allowed[origin] {
			fail(c, http.StatusForbidden, "bad_origin",
				"This request did not come from a recognised origin.")
			return
		}
		c.Next()
	}
}

// ---------------------------------------------------------------- rate limit

// limiter is a fixed-window counter. Deliberately in memory: the limits here
// protect a single-user server against brute force and runaway clients, and
// losing the counters on restart is not a meaningful weakness.
type limiter struct {
	mu      sync.Mutex
	windows map[string]*window
	limit   int
	period  time.Duration
}

type window struct {
	count int
	reset time.Time
}

func newLimiter(limit int, period time.Duration) *limiter {
	l := &limiter{windows: map[string]*window{}, limit: limit, period: period}
	go l.reap()
	return l
}

func (l *limiter) allow(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.windows[key]
	if !ok || now.After(w.reset) {
		l.windows[key] = &window{count: 1, reset: now.Add(l.period)}
		return true, 0
	}
	if w.count >= l.limit {
		return false, time.Until(w.reset)
	}
	w.count++
	return true, 0
}

func (l *limiter) reap() {
	for range time.Tick(5 * time.Minute) {
		now := time.Now()
		l.mu.Lock()
		for k, w := range l.windows {
			if now.After(w.reset) {
				delete(l.windows, k)
			}
		}
		l.mu.Unlock()
	}
}

// rateLimit keys on the client IP, or on the authenticated user when there is
// one — a per-IP limit alone is the wrong unit behind CGNAT.
func (s *Server) rateLimit(l *limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.ClientIP()
		if u := currentUser(c); u != nil {
			key = "u" + strconv.FormatInt(u.ID, 10)
		}
		if ok, retry := l.allow(key); !ok {
			c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			fail(c, http.StatusTooManyRequests, "rate_limited",
				"Too many attempts. Try again in "+retry.Round(time.Second).String()+".")
			return
		}
		c.Next()
	}
}

// ---------------------------------------------------------------- logging

func requestLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		// Never log the query string, and redact the path: a share token is a
		// bearer credential that sits in the path itself, so logging /s/:token
		// verbatim would put working links in the log file.
		attrs := []any{
			"method", c.Request.Method,
			"path", redactPath(c.Request.URL.Path),
			"status", c.Writer.Status(),
			"ms", time.Since(start).Milliseconds(),
			"ip", c.ClientIP(),
		}
		switch {
		case c.Writer.Status() >= 500:
			log.Error("request", attrs...)
		case c.Writer.Status() >= 400:
			log.Warn("request", attrs...)
		default:
			log.Info("request", attrs...)
		}
	}
}

// redactPath strips the token out of a share URL before it reaches a log.
func redactPath(path string) string {
	if strings.HasPrefix(path, "/s/") && len(path) > 3 {
		return "/s/<token>"
	}
	return path
}

func recovery(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, err any) {
		log.Error("panic recovered", "err", err, "path", redactPath(c.Request.URL.Path))
		fail(c, http.StatusInternalServerError, "internal", "Something went wrong. Try again.")
	})
}

// securityHeaders applies the headers that cost nothing and close real holes.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		c.Next()
	}
}
