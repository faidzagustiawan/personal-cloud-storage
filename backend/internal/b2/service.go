package b2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudapp/internal/config"
	"cloudapp/internal/store"
)

// Service is the application-facing storage layer: it knows the object layout,
// caches download authorizations, and enforces which key is allowed to mint
// which credential.
//
// Handlers use this. They never touch Client directly.

const (
	// LargeFileCutoff is where the browser switches from a single
	// b2_upload_file to the multipart API (spec §5.3).
	LargeFileCutoff = 100 << 20
	// PartSize keeps a 500 MB upload at 50 parts, far under B2's 10,000-part
	// ceiling, and keeps peak browser memory near 4 x 10 MB.
	PartSize = 10 << 20

	// downloadTokenTTL is B2's maximum: seven days.
	downloadTokenTTL = 7 * 24 * time.Hour
	// refreshBefore renews a token while a day of validity remains, so no
	// request ever races an expiry.
	refreshBefore = 24 * time.Hour
)

var (
	ErrNotConfigured  = errors.New("b2 is not configured")
	ErrNoUploadKey    = errors.New("no restricted upload key configured")
	ErrSizeMismatch   = errors.New("uploaded size does not match what was declared")
	ErrDigestMismatch = errors.New("uploaded content hash does not match what was declared")
)

type Service struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger

	master *Client // full access; never leaves the VPS
	upload *Client // writeFiles only, prefix-restricted; its tokens go to the browser

	// allowMasterUpload is an explicit, loudly logged development escape hatch.
	allowMasterUpload bool

	mu     sync.Mutex
	tokens map[string]*store.CachedToken // in-process mirror of the b2_tokens table
}

func NewService(cfg *config.Config, st *store.Store, log *slog.Logger, allowMasterUpload bool) *Service {
	s := &Service{
		cfg:               cfg,
		store:             st,
		log:               log,
		allowMasterUpload: allowMasterUpload,
		tokens:            map[string]*store.CachedToken{},
	}
	if cfg.B2Configured() {
		s.master = NewClient("master", cfg.B2KeyID, cfg.B2AppKey)
	}
	if cfg.B2UploadKeyID != "" && cfg.B2UploadKey != "" {
		s.upload = NewClient("upload", cfg.B2UploadKeyID, cfg.B2UploadKey)
	}
	if cfg.B2APIBase != "" {
		for _, c := range []*Client{s.master, s.upload} {
			if c != nil {
				c.authBase = cfg.B2APIBase
			}
		}
	}
	return s
}

func (s *Service) Configured() bool { return s.master != nil }

// ---------------------------------------------------------------- object layout

// Spec §3. object_key is 32 random hex characters, so these names are
// unguessable even before the download authorization is considered.

func OrigName(userID int64, objectKey, ext string) string {
	return path.Join(userPrefix(userID), "orig", objectKey+normalizeExt(ext))
}

func ThumbName(userID int64, objectKey string) string {
	return path.Join(userPrefix(userID), "thumb", objectKey+".webp")
}

func MetaName(userID int64, objectKey string) string {
	return path.Join(userPrefix(userID), "meta", objectKey+".json")
}

func BackupName(userID int64, day string) string {
	return path.Join(userPrefix(userID), "db", "cloud-"+day+".sqlite")
}

func userPrefix(userID int64) string {
	return "users/" + strconv.FormatInt(userID, 10)
}

// normalizeExt reduces a client-supplied extension to a safe suffix.
//
// The object key itself is server-generated random hex, but the extension is
// derived from a filename the client chose. Stripping to alphanumerics is
// blunt and correct: real extensions are alphanumeric, and anything else was
// either junk or an attempt to escape the key's position in the hierarchy.
func normalizeExt(ext string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(ext)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 11 {
			break
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "." + b.String()
}

// ---------------------------------------------------------------- browser credentials

// uploadClient returns the key whose tokens may be handed to a browser.
//
// This is the security boundary that makes decision D1 acceptable: a leaked
// upload token can write under one user's prefix and can neither read, list
// nor delete. Falling back to the master key silently would hand a browser
// full bucket access, so it requires an explicit opt-in and is logged every
// single time it is used.
func (s *Service) uploadClient() (*Client, error) {
	if s.upload != nil {
		return s.upload, nil
	}
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	if s.allowMasterUpload {
		s.log.Warn("minting a browser upload token from the MASTER key — full bucket access is being handed to a client; set B2_UPLOAD_KEY_ID for anything but local development")
		return s.master, nil
	}
	return nil, ErrNoUploadKey
}

// UploadCredentials mints what the browser needs for a single small upload.
func (s *Service) UploadCredentials(ctx context.Context, name string) (*UploadTarget, error) {
	c, err := s.uploadClient()
	if err != nil {
		return nil, err
	}
	return c.GetUploadURL(ctx, s.cfg.B2BucketID)
}

// StartLargeUpload opens a multipart upload. The master key owns the lifecycle
// calls; only the per-part credentials cross to the browser.
func (s *Service) StartLargeUpload(ctx context.Context, name, contentType, wholeSha1 string) (string, error) {
	if !s.Configured() {
		return "", ErrNotConfigured
	}
	info, err := s.master.StartLargeFile(ctx, s.cfg.B2BucketID, name, contentType, wholeSha1)
	if err != nil {
		return "", err
	}
	return info.FileID, nil
}

// PartCredentials mints n part-upload targets.
//
// One per concurrent worker, because a B2 upload URL handles one upload at a
// time. They are fetched in parallel since each is a separate round trip and
// the browser is waiting on all of them.
func (s *Service) PartCredentials(ctx context.Context, largeFileID string, n int) ([]*UploadTarget, error) {
	c, err := s.uploadClient()
	if err != nil {
		return nil, err
	}
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}

	targets := make([]*UploadTarget, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			targets[i], errs[i] = c.GetUploadPartURL(ctx, largeFileID)
		}(i)
	}
	wg.Wait()

	out := targets[:0]
	for i, t := range targets {
		if errs[i] != nil {
			// One failure is survivable: the browser uses fewer workers.
			s.log.Warn("could not mint a part upload url", "err", errs[i])
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no part upload urls available: %w", errors.Join(errs...))
	}
	return out, nil
}

func (s *Service) FinishLargeUpload(ctx context.Context, largeFileID string, partSha1s []string) (*FileInfo, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	return s.master.FinishLargeFile(ctx, largeFileID, partSha1s)
}

func (s *Service) CancelLargeUpload(ctx context.Context, largeFileID string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	return s.master.CancelLargeFile(ctx, largeFileID)
}

// ---------------------------------------------------------------- verification

// Verify confirms that what B2 actually stored matches what the client claimed
// (spec §4.2 step 2).
//
// Without this the browser could declare a 1 KB upload, store 4 GB, and walk
// straight through the quota. It is the counterweight to letting the client
// talk to B2 directly.
func (s *Service) Verify(ctx context.Context, fileID string, expectSize int64, expectSha1 string) (*FileInfo, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	info, err := s.master.GetFileInfo(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if info.ContentLength != expectSize {
		return nil, fmt.Errorf("%w: b2 has %d bytes, client declared %d",
			ErrSizeMismatch, info.ContentLength, expectSize)
	}
	if expectSha1 != "" {
		if got := info.WholeSha1(); got != "" && !strings.EqualFold(got, expectSha1) {
			return nil, fmt.Errorf("%w: b2 has %s, client declared %s", ErrDigestMismatch, got, expectSha1)
		}
	}
	return info, nil
}

// ---------------------------------------------------------------- downloads

// DownloadURL composes a ready-to-use URL for an object belonging to userID.
//
// The token is prefix-scoped and cached for seven days, so this costs no B2
// transaction on the common path and produces a stable query string that
// Cloudflare can cache against (spec §2.4, §2.5).
func (s *Service) DownloadURL(ctx context.Context, userID int64, name string) (string, error) {
	token, err := s.prefixToken(ctx, userID)
	if err != nil {
		return "", err
	}
	return s.composeURL(ctx, name, token)
}

// URLBuilder fetches the user's prefix token once and returns a closure that
// composes URLs without further I/O.
//
// A gallery page needs a URL for every thumbnail and original on it. Calling
// DownloadURL in a loop would work — the token is cached — but this makes the
// "one token, many URLs" property explicit at the call site, and keeps the
// error handling at the top rather than inside the row loop.
func (s *Service) URLBuilder(ctx context.Context, userID int64) (func(name string) string, error) {
	token, err := s.prefixToken(ctx, userID)
	if err != nil {
		return nil, err
	}
	if s.cfg.CDNBase != "" {
		base := s.cfg.CDNBase
		return func(name string) string {
			return base + "/" + EscapeName(name) + "?Authorization=" + token
		}, nil
	}
	auth, err := s.master.Auth(ctx)
	if err != nil {
		return nil, err
	}
	base := auth.DownloadURL + "/file/" + s.cfg.B2BucketName
	return func(name string) string {
		return base + "/" + EscapeName(name) + "?Authorization=" + token
	}, nil
}

// SignedURL issues a short-lived URL for a single object.
//
// Share links use this rather than the cached prefix token: a recipient can
// save whatever URL they are given, so revoking a share must not have to wait
// out a seven-day token (spec §4.5).
func (s *Service) SignedURL(ctx context.Context, name string, ttl time.Duration) (string, error) {
	if !s.Configured() {
		return "", ErrNotConfigured
	}
	seconds := int(ttl.Seconds())
	if seconds < 60 {
		seconds = 60
	}
	token, err := s.master.GetDownloadAuthorization(ctx, s.cfg.B2BucketID, name, seconds)
	if err != nil {
		return "", err
	}
	return s.composeURL(ctx, name, token)
}

func (s *Service) composeURL(ctx context.Context, name, token string) (string, error) {
	escaped := EscapeName(name)
	if s.cfg.CDNBase != "" {
		return s.cfg.CDNBase + "/" + escaped + "?Authorization=" + token, nil
	}
	auth, err := s.master.Auth(ctx)
	if err != nil {
		return "", err
	}
	return auth.DownloadURL + "/file/" + s.cfg.B2BucketName + "/" + escaped + "?Authorization=" + token, nil
}

// prefixToken returns the cached seven-day authorization covering users/<id>/,
// minting one if none is valid. Memory first, then the database, then B2.
func (s *Service) prefixToken(ctx context.Context, userID int64) (string, error) {
	if !s.Configured() {
		return "", ErrNotConfigured
	}
	prefix := userPrefix(userID) + "/"
	scope := "download:" + prefix

	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.tokens[scope]; ok && time.Until(t.ExpiresAt) > refreshBefore {
		return t.Token, nil
	}

	if t, err := s.store.GetB2Token(ctx, scope); err != nil {
		s.log.Warn("reading cached b2 token failed", "scope", scope, "err", err)
	} else if t != nil && time.Until(t.ExpiresAt) > refreshBefore {
		s.tokens[scope] = t
		return t.Token, nil
	}

	token, err := s.master.GetDownloadAuthorization(ctx, s.cfg.B2BucketID, prefix, int(downloadTokenTTL.Seconds()))
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(downloadTokenTTL)

	cached := &store.CachedToken{Scope: scope, Token: token, ExpiresAt: expires}
	s.tokens[scope] = cached
	if err := s.store.PutB2Token(ctx, scope, token, expires); err != nil {
		// Losing the persistent copy costs one extra transaction after a
		// restart, not correctness.
		s.log.Warn("caching b2 token failed", "scope", scope, "err", err)
	}
	s.log.Info("minted b2 download authorization", "prefix", prefix, "valid_days", 7)
	return token, nil
}

// InvalidatePrefixToken drops the cached authorization for a user, so the next
// URL is built from a freshly minted one.
func (s *Service) InvalidatePrefixToken(ctx context.Context, userID int64) {
	scope := "download:" + userPrefix(userID) + "/"
	s.mu.Lock()
	delete(s.tokens, scope)
	s.mu.Unlock()
	if err := s.store.DeleteB2Token(ctx, scope); err != nil {
		s.log.Warn("clearing cached b2 token failed", "scope", scope, "err", err)
	}
}

// ---------------------------------------------------------------- server-side objects

// PutSidecar writes the per-file recovery JSON of spec §4.2/§7.3. If both the
// VPS and every database backup are lost, listing the meta/ prefix rebuilds
// the entire index.
func (s *Service) PutSidecar(ctx context.Context, userID int64, objectKey string, body []byte) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	_, err := s.master.PutObject(ctx, s.cfg.B2BucketID, MetaName(userID, objectKey), "application/json", body)
	return err
}

func (s *Service) PutBackup(ctx context.Context, userID int64, day string, body []byte) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	_, err := s.master.PutObject(ctx, s.cfg.B2BucketID, BackupName(userID, day), "application/x-sqlite3", body)
	return err
}

// Delete removes an object version permanently. Idempotent.
func (s *Service) Delete(ctx context.Context, name, fileID string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	if fileID == "" {
		return nil
	}
	return s.master.DeleteFileVersion(ctx, name, fileID)
}

// List pages object names under a prefix, for reindex --from-b2 only.
func (s *Service) List(ctx context.Context, prefix, startFrom string, limit int) ([]FileInfo, string, error) {
	if !s.Configured() {
		return nil, "", ErrNotConfigured
	}
	return s.master.ListNames(ctx, s.cfg.B2BucketID, prefix, startFrom, limit)
}

// ---------------------------------------------------------------- diagnostics

// Status describes the storage layer for the health endpoint (spec §4.6).
type Status struct {
	State          string   `json:"state"` // unconfigured | ok | error
	Detail         string   `json:"detail,omitempty"`
	UploadKeyScope string   `json:"upload_key_scope,omitempty"`
	Capabilities   []string `json:"upload_key_capabilities,omitempty"`
}

// Status reports whether B2 actually answers, and whether the upload key is
// scoped the way spec §2.3 requires. Both authorizations are cached, so this
// costs nothing after the first call.
func (s *Service) Status(ctx context.Context) Status {
	if !s.Configured() {
		return Status{State: "unconfigured"}
	}
	if _, err := s.master.Auth(ctx); err != nil {
		return Status{State: "error", Detail: err.Error()}
	}

	st := Status{State: "ok"}
	if s.upload == nil {
		st.Detail = "no restricted upload key: browser uploads are refused"
		return st
	}

	auth, err := s.upload.Auth(ctx)
	if err != nil {
		return Status{State: "error", Detail: "upload key: " + err.Error()}
	}
	st.Capabilities = auth.Allowed.Capabilities
	if auth.Allowed.NamePrefix != nil {
		st.UploadKeyScope = *auth.Allowed.NamePrefix
	}

	// A misconfigured upload key is a silent security downgrade, so it is
	// surfaced rather than left to be discovered later.
	var problems []string
	if st.UploadKeyScope == "" {
		problems = append(problems, "upload key has no namePrefix restriction")
	}
	for _, forbidden := range []string{"readFiles", "deleteFiles", "listFiles"} {
		if auth.Allowed.Has(forbidden) {
			problems = append(problems, "upload key holds "+forbidden)
		}
	}
	if len(problems) > 0 {
		st.State = "error"
		st.Detail = strings.Join(problems, "; ")
	}
	return st
}

// GetObject reads an object into memory. Maintenance only — see Client.GetObject.
func (s *Service) GetObject(ctx context.Context, name string) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	return s.master.GetObject(ctx, s.cfg.B2BucketName, name)
}

// PutThumbnail uploads a thumbnail produced on the server by the tier 3 worker.
func (s *Service) PutThumbnail(ctx context.Context, userID int64, objectKey string, body []byte, contentType string) error {
	if !s.Configured() {
		return ErrNotConfigured
	}
	_, err := s.master.PutObject(ctx, s.cfg.B2BucketID, ThumbName(userID, objectKey), contentType, body)
	return err
}

// BucketName is needed by callers that compose their own storage paths.
func (s *Service) BucketName() string { return s.cfg.B2BucketName }
