package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// B2 download authorizations are cached in the database as well as in memory
// so that a restart does not re-spend a Class C transaction (spec §2.4). They
// are valid for seven days; a server that restarts a few times an hour during
// a deploy would otherwise burn through the free tier for no reason.

type CachedToken struct {
	Scope     string
	Token     string
	ExpiresAt time.Time
}

func (s *Store) GetB2Token(ctx context.Context, scope string) (*CachedToken, error) {
	var t CachedToken
	var expires int64
	err := s.R.QueryRowContext(ctx,
		`SELECT scope, token, expires_at FROM b2_tokens WHERE scope = ?`, scope).
		Scan(&t.Scope, &t.Token, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.ExpiresAt = time.Unix(expires, 0).UTC()
	return &t, nil
}

func (s *Store) PutB2Token(ctx context.Context, scope, token string, expiresAt time.Time) error {
	_, err := s.W.ExecContext(ctx,
		`INSERT INTO b2_tokens (scope, token, expires_at) VALUES (?, ?, ?)
		 ON CONFLICT(scope) DO UPDATE SET token = excluded.token, expires_at = excluded.expires_at`,
		scope, token, expiresAt.Unix())
	return err
}

// DeleteB2Token drops a cached token, forcing the next request to mint a fresh
// one. Called when B2 rejects a token we believed was still valid.
func (s *Store) DeleteB2Token(ctx context.Context, scope string) error {
	_, err := s.W.ExecContext(ctx, `DELETE FROM b2_tokens WHERE scope = ?`, scope)
	return err
}
