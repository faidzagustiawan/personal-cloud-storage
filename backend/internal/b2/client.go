// Package b2 talks to the Backblaze B2 native API.
//
// No SDK: the raw calls are few, the error and retry semantics matter, and the
// shapes were already proven end-to-end by tools/b2probe against a live bucket.
//
// The type here is deliberately low-level — authorization caching, retries and
// URL composition. Anything that knows about users, object layout or the
// database belongs in Service (service.go).
package b2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase = "https://api.backblazeb2.com"
	apiVersion     = "/b2api/v2"

	// B2 authorization tokens last 24 hours. Refreshing at 12 keeps a wide
	// margin without spending meaningful Class C transactions: two per day.
	authLifetime = 12 * time.Hour

	// authFailureTTL is how long a failed authorization is remembered.
	//
	// Without this, nothing is cached when credentials are wrong, so every
	// health check re-attempts authorization against B2. A monitor polling
	// every ten seconds would issue nearly nine thousand failing calls a day
	// and invite rate limiting from the far end. Thirty seconds keeps recovery
	// prompt after the credentials are fixed while collapsing the stampede.
	authFailureTTL = 30 * time.Second
)

// Error is a structured B2 API error.
type Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("b2 %d %s: %s", e.Status, e.Code, e.Message)
}

// Retryable reports whether repeating the request could plausibly succeed.
func (e *Error) Retryable() bool {
	return e.Status == 408 || e.Status == 429 || e.Status >= 500
}

// ExpiredAuth reports whether the failure was the authorization token rather
// than the request. These are retried once after re-authorizing.
func (e *Error) ExpiredAuth() bool {
	return e.Status == 401 &&
		(e.Code == "expired_auth_token" || e.Code == "bad_auth_token" || e.Code == "unauthorized")
}

func AsError(err error) (*Error, bool) {
	var be *Error
	ok := errors.As(err, &be)
	return be, ok
}

// Allowed mirrors the restrictions attached to the application key in use.
type Allowed struct {
	Capabilities []string `json:"capabilities"`
	BucketID     string   `json:"bucketId"`
	BucketName   string   `json:"bucketName"`
	NamePrefix   *string  `json:"namePrefix"`
}

func (a Allowed) Has(capability string) bool {
	for _, c := range a.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

type authState struct {
	Token       string
	APIURL      string
	DownloadURL string
	AccountID   string
	Allowed     Allowed
	MinPartSize int64
	RecPartSize int64
	obtained    time.Time
}

type authResponse struct {
	AccountID           string  `json:"accountId"`
	AuthorizationToken  string  `json:"authorizationToken"`
	APIURL              string  `json:"apiUrl"`
	DownloadURL         string  `json:"downloadUrl"`
	RecommendedPartSize int64   `json:"recommendedPartSize"`
	AbsoluteMinPartSize int64   `json:"absoluteMinimumPartSize"`
	Allowed             Allowed `json:"allowed"`
}

// Client holds one application key and its cached authorization.
//
// Two of these exist in a running server: the master key, which never leaves
// the VPS, and the write-only prefix-restricted upload key whose derived
// tokens are handed to the browser (spec §2.3).
type Client struct {
	keyID  string
	appKey string
	name   string // for error messages: "master" or "upload"
	http   *http.Client

	// authBase is the b2_authorize_account host. Every other call goes to the
	// apiUrl that authorization returns, so this is the only fixed endpoint —
	// and the only seam the tests need to point at a fake B2.
	authBase string

	mu       sync.Mutex
	auth     *authState
	authErr  error
	authFail time.Time
}

func NewClient(name, keyID, appKey string) *Client {
	return &Client{
		keyID:    keyID,
		appKey:   appKey,
		name:     name,
		http:     &http.Client{Timeout: 60 * time.Second},
		authBase: defaultAPIBase,
	}
}

// Auth returns a valid authorization, refreshing it if necessary.
func (c *Client) Auth(ctx context.Context) (*authState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authLocked(ctx)
}

func (c *Client) authLocked(ctx context.Context) (*authState, error) {
	if c.auth != nil && time.Since(c.auth.obtained) < authLifetime {
		return c.auth, nil
	}
	if c.authErr != nil && time.Since(c.authFail) < authFailureTTL {
		return nil, c.authErr
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.authBase+apiVersion+"/b2_authorize_account", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.keyID, c.appKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.failAuth(fmt.Errorf("b2 authorize (%s key): %w", c.name, err))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, c.failAuth(fmt.Errorf("b2 authorize (%s key): %w", c.name, decodeError(resp.StatusCode, body)))
	}

	var ar authResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, c.failAuth(fmt.Errorf("b2 authorize (%s key): decode response: %w", c.name, err))
	}

	c.auth = &authState{
		Token:       ar.AuthorizationToken,
		APIURL:      ar.APIURL,
		DownloadURL: ar.DownloadURL,
		AccountID:   ar.AccountID,
		Allowed:     ar.Allowed,
		MinPartSize: ar.AbsoluteMinPartSize,
		RecPartSize: ar.RecommendedPartSize,
		obtained:    time.Now(),
	}
	c.authErr, c.authFail = nil, time.Time{}
	return c.auth, nil
}

// failAuth records an authorization failure so that repeated callers — chiefly
// the health endpoint — do not each open their own round trip to B2. Caller
// must hold c.mu.
func (c *Client) failAuth(err error) error {
	c.authErr, c.authFail = err, time.Now()
	return err
}

// invalidate forces the next call to re-authorize. Called when B2 rejects the
// token we believed was still good.
func (c *Client) invalidate() {
	c.mu.Lock()
	c.auth = nil
	// An expired token is not a credential failure: clear the negative cache
	// so the retry actually re-authorizes instead of replaying a stale error.
	c.authErr, c.authFail = nil, time.Time{}
	c.mu.Unlock()
}

// Call performs an authenticated JSON POST against the account's API host,
// retrying transient failures and re-authorizing once on an expired token.
func (c *Client) Call(ctx context.Context, endpoint string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return err
			}
		}

		auth, err := c.Auth(ctx)
		if err != nil {
			lastErr = err
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			auth.APIURL+apiVersion+"/"+endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", auth.Token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("%s: %w", endpoint, err)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("%s: read response: %w", endpoint, readErr)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			if out == nil {
				return nil
			}
			return json.Unmarshal(body, out)
		}

		apiErr := decodeError(resp.StatusCode, body)
		lastErr = fmt.Errorf("%s: %w", endpoint, apiErr)

		if be, ok := AsError(apiErr); ok {
			if be.ExpiredAuth() {
				c.invalidate()
				continue
			}
			if be.Retryable() {
				continue
			}
		}
		return lastErr
	}
	return lastErr
}

func decodeError(status int, body []byte) error {
	var be Error
	if json.Unmarshal(body, &be) == nil && be.Code != "" {
		be.Status = status
		return &be
	}
	return &Error{Status: status, Code: "http_error", Message: strings.TrimSpace(string(body))}
}

// backoff is a variable so tests can collapse it; retry behaviour is worth
// testing, waiting six seconds to test it is not.
var backoff = func(attempt int) time.Duration {
	return time.Duration(1<<attempt) * time.Second
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// EscapeName percent-encodes a B2 object name segment by segment, leaving the
// "/" separators literal.
//
// url.PathEscape on the whole name escapes "/" to %2F, which would collapse
// users/1/orig/abc.jpg into a single flat filename containing slashes. The
// bug is invisible in a smoke test with a top-level key and breaks every real
// upload.
func EscapeName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
