package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNoShare      = errors.New("share not found")
	ErrShareExpired = errors.New("share has expired")
)

// Shares are public links: anyone holding the token can fetch the file, with no
// session. The token is therefore the whole credential — 256 bits of
// crypto/rand, never derived from the file id, and never written to a log.

const shareTokenBytes = 32

// MaxShareDays is bounded because a share is a standing grant. Seven days also
// happens to be the ceiling B2 puts on a download authorization.
const MaxShareDays = 30

type Share struct {
	ID        int64
	UserID    int64
	FileID    int64
	Token     string
	ExpiresAt int64
	RevokedAt *int64
	ViewCount int64
	CreatedAt int64

	// Joined from files, for the management list.
	Filename string
	Kind     string
}

func (s *Share) Active(now time.Time) bool {
	return s.RevokedAt == nil && s.ExpiresAt > now.Unix()
}

// CreateShare mints a link for a file the user owns.
func (s *Store) CreateShare(ctx context.Context, userID, fileID int64, days int) (*Share, error) {
	if days <= 0 {
		days = 7
	}
	if days > MaxShareDays {
		return nil, fmt.Errorf("a share can last at most %d days", MaxShareDays)
	}

	// Ownership is checked here rather than trusted from the handler, and a
	// trashed file cannot be shared: the link would break the moment the purge
	// job ran.
	var ok int
	if err := s.R.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files
		 WHERE id = ? AND user_id = ? AND is_deleted = 0 AND status = 'ready'`,
		fileID, userID).Scan(&ok); err != nil {
		return nil, err
	}
	if ok == 0 {
		return nil, ErrNoFile
	}

	raw := make([]byte, shareTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()

	res, err := s.W.ExecContext(ctx,
		`INSERT INTO shares (user_id, file_id, token, expires_at, created_at)
		 VALUES (?, ?, ?, ?, unixepoch())`,
		userID, fileID, token, expires)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.ShareByID(ctx, userID, id)
}

func (s *Store) ShareByID(ctx context.Context, userID, id int64) (*Share, error) {
	return scanShare(s.R.QueryRowContext(ctx, shareSelect+` WHERE s.id = ? AND s.user_id = ?`, id, userID))
}

const shareSelect = `
	SELECT s.id, s.user_id, s.file_id, s.token, s.expires_at, s.revoked_at,
	       s.view_count, s.created_at, f.filename, f.kind
	FROM shares s JOIN files f ON f.id = s.file_id`

func (s *Store) ListShares(ctx context.Context, userID int64) ([]*Share, error) {
	rows, err := s.R.QueryContext(ctx,
		shareSelect+` WHERE s.user_id = ? ORDER BY s.created_at DESC LIMIT 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Share
	for rows.Next() {
		share, err := scanShareRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, share)
	}
	return out, rows.Err()
}

// ResolveShare looks up a token for the public endpoint.
//
// Returns the file alongside the share so the caller can mint a URL without a
// second query, and refuses anything revoked, expired, or trashed.
func (s *Store) ResolveShare(ctx context.Context, token string) (*Share, *File, error) {
	if token == "" {
		return nil, nil, ErrNoShare
	}

	var share Share
	var revoked sql.NullInt64
	err := s.R.QueryRowContext(ctx,
		`SELECT id, user_id, file_id, expires_at, revoked_at FROM shares WHERE token = ?`, token).
		Scan(&share.ID, &share.UserID, &share.FileID, &share.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNoShare
	}
	if err != nil {
		return nil, nil, err
	}
	if revoked.Valid {
		return nil, nil, ErrNoShare
	}
	if share.ExpiresAt <= time.Now().Unix() {
		return nil, nil, ErrShareExpired
	}

	file, err := s.FileByID(ctx, share.UserID, share.FileID)
	if err != nil {
		return nil, nil, err
	}
	// A file in the trash is on its way to being purged, so its links stop
	// working now rather than breaking silently in thirty days.
	if file.IsDeleted || file.Status != "ready" {
		return nil, nil, ErrNoShare
	}
	return &share, file, nil
}

// TouchShare records a view. Best-effort: a failed counter must never stop a
// download from being served.
func (s *Store) TouchShare(ctx context.Context, id int64) {
	_, _ = s.W.ExecContext(ctx, `UPDATE shares SET view_count = view_count + 1 WHERE id = ?`, id)
}

// RevokeShare takes effect immediately, which is the whole reason share
// downloads are redirected through this server rather than handed out as a
// long-lived storage URL (spec §4.5).
func (s *Store) RevokeShare(ctx context.Context, userID, id int64) error {
	res, err := s.W.ExecContext(ctx,
		`UPDATE shares SET revoked_at = unixepoch()
		 WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoShare
	}
	return nil
}

// PurgeDeadShares drops rows that can no longer grant anything.
func (s *Store) PurgeDeadShares(ctx context.Context) (int64, error) {
	res, err := s.W.ExecContext(ctx,
		`DELETE FROM shares
		 WHERE expires_at < unixepoch() - 604800
		    OR (revoked_at IS NOT NULL AND revoked_at < unixepoch() - 604800)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanShare(row *sql.Row) (*Share, error) {
	share, err := scanShareInto(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoShare
	}
	return share, err
}

func scanShareRow(rows *sql.Rows) (*Share, error) { return scanShareInto(rows.Scan) }

func scanShareInto(scan func(...any) error) (*Share, error) {
	var s Share
	var revoked sql.NullInt64
	if err := scan(&s.ID, &s.UserID, &s.FileID, &s.Token, &s.ExpiresAt, &revoked,
		&s.ViewCount, &s.CreatedAt, &s.Filename, &s.Kind); err != nil {
		return nil, err
	}
	if revoked.Valid {
		s.RevokedAt = &revoked.Int64
	}
	return &s, nil
}
