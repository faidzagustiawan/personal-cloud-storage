package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Sessions are opaque random tokens kept in the database (decision D3), not
// JWTs. A single VPS with SQLite already present gains nothing from stateless
// tokens, and loses the two things that actually matter here: logout that
// really logs out, and per-session revocation from the settings page.
//
// Only sha256(token) is stored. A database leak therefore does not hand the
// attacker usable sessions.

var ErrNoSession = errors.New("session not found or expired")

const (
	sessionTokenBytes = 32 // 256 bits
	// slideAfter is how much of the lifetime must elapse before an active
	// session has its expiry extended. Sliding on every request would mean a
	// database write per request for no benefit.
	slideAfter = 24 * time.Hour
)

type Session struct {
	TokenHash string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
}

// NewSession mints a token, stores its hash, and returns the raw token. The
// raw value exists only in this return and in the client's cookie.
func (s *Store) NewSession(ctx context.Context, userID int64, ttl time.Duration, userAgent string) (string, time.Time, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(ttl).UTC()

	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}

	_, err := s.W.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, user_agent)
		 VALUES (?, ?, unixepoch(), ?, ?)`,
		hashToken(token), userID, expires.Unix(), userAgent)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// LookupSession resolves a raw token to its user, rejecting expired and
// revoked sessions, and slides the expiry of one that is still in active use.
func (s *Store) LookupSession(ctx context.Context, token string, ttl time.Duration) (*User, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	h := hashToken(token)

	var userID, expires int64
	var revoked sql.NullInt64
	err := s.R.QueryRowContext(ctx,
		`SELECT user_id, expires_at, revoked_at FROM sessions WHERE token_hash = ?`, h).
		Scan(&userID, &expires, &revoked)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid {
		return nil, ErrNoSession
	}

	now := time.Now()
	if now.Unix() >= expires {
		return nil, ErrNoSession
	}

	if time.Until(time.Unix(expires, 0)) < ttl-slideAfter {
		if _, err := s.W.ExecContext(ctx,
			`UPDATE sessions SET expires_at = ? WHERE token_hash = ? AND revoked_at IS NULL`,
			now.Add(ttl).Unix(), h); err != nil {
			return nil, err
		}
	}

	return s.UserByID(ctx, userID)
}

func (s *Store) RevokeSession(ctx context.Context, token string) error {
	_, err := s.W.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = unixepoch()
		 WHERE token_hash = ? AND revoked_at IS NULL`, hashToken(token))
	return err
}

// RevokeAllSessions is what a password change must call — otherwise a stolen
// session survives the very action taken to lock the attacker out.
func (s *Store) RevokeAllSessions(ctx context.Context, userID int64) (int64, error) {
	res, err := s.W.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = unixepoch()
		 WHERE user_id = ? AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeExpiredSessions drops rows that can no longer authenticate anything.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.W.ExecContext(ctx,
		`DELETE FROM sessions
		 WHERE expires_at < unixepoch()
		    OR (revoked_at IS NOT NULL AND revoked_at < unixepoch() - 604800)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- listing

// SessionInfo is what the settings page shows. The raw token is never stored,
// so there is nothing here that could be replayed to authenticate.
type SessionInfo struct {
	ID        string // the token hash: a digest, not a credential
	CreatedAt int64
	ExpiresAt int64
	UserAgent string
	Current   bool
}

func (s *Store) ListSessions(ctx context.Context, userID int64, currentToken string) ([]*SessionInfo, error) {
	rows, err := s.R.QueryContext(ctx,
		`SELECT token_hash, created_at, expires_at, COALESCE(user_agent, '')
		 FROM sessions
		 WHERE user_id = ? AND revoked_at IS NULL AND expires_at > unixepoch()
		 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	current := hashToken(currentToken)
	var out []*SessionInfo
	for rows.Next() {
		var info SessionInfo
		if err := rows.Scan(&info.ID, &info.CreatedAt, &info.ExpiresAt, &info.UserAgent); err != nil {
			return nil, err
		}
		info.Current = info.ID == current
		out = append(out, &info)
	}
	return out, rows.Err()
}

// RevokeSessionByID revokes one session by its token hash. Scoped to the owner
// so one account cannot sign another out.
func (s *Store) RevokeSessionByID(ctx context.Context, userID int64, id string) error {
	res, err := s.W.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = unixepoch()
		 WHERE token_hash = ? AND user_id = ? AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSession
	}
	return nil
}

// RevokeOtherSessions signs out everything except the caller's own session.
func (s *Store) RevokeOtherSessions(ctx context.Context, userID int64, keepToken string) (int64, error) {
	res, err := s.W.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = unixepoch()
		 WHERE user_id = ? AND token_hash != ? AND revoked_at IS NULL`,
		userID, hashToken(keepToken))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
