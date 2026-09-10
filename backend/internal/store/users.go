package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is 12 per spec §8. Roughly 250 ms per hash on a small VPS, which
// is the point: it is the rate limiter of last resort on the login endpoint.
const bcryptCost = 12

var (
	ErrNoUser        = errors.New("user not found")
	ErrBadCredential = errors.New("invalid username or password")
	ErrUserExists    = errors.New("username already taken")
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

type User struct {
	ID           int64
	Username     string
	StorageUsed  int64
	StorageQuota int64
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

// CreateUser is reachable only from the CLI. There is no registration endpoint
// (decision D7) — an open /register on a personal cloud is free hosting.
func (s *Store) CreateUser(ctx context.Context, username, password string, quota int64) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return nil, fmt.Errorf("username must be 2-32 chars of a-z, 0-9, dot, dash or underscore")
	}
	if len([]rune(password)) < 10 {
		return nil, fmt.Errorf("password must be at least 10 characters")
	}
	if len(password) > 72 {
		// bcrypt silently truncates past 72 bytes, which would make the tail of
		// a long passphrase decorative. Refuse instead of quietly weakening it.
		return nil, fmt.Errorf("password must be at most 72 bytes (bcrypt truncates beyond that)")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	res, err := s.W.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, storage_quota, created_at)
		 VALUES (?, ?, ?, unixepoch())`,
		username, string(hash), quota)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrUserExists
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, id)
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.scanUser(s.R.QueryRowContext(ctx,
		`SELECT id, username, storage_used, storage_quota, created_at, last_login_at
		 FROM users WHERE id = ?`, id))
}

// Authenticate verifies a password and returns the user.
//
// A missing user still pays for a bcrypt comparison against a dummy hash, so
// that response time does not reveal which usernames exist.
func (s *Store) Authenticate(ctx context.Context, username, password string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))

	var id int64
	var hash string
	err := s.R.QueryRowContext(ctx,
		`SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &hash)

	if errors.Is(err, sql.ErrNoRows) {
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return nil, ErrBadCredential
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return nil, ErrBadCredential
	}

	if _, err := s.W.ExecContext(ctx,
		`UPDATE users SET last_login_at = unixepoch() WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return s.UserByID(ctx, id)
}

func (s *Store) SetPassword(ctx context.Context, userID int64, password string) error {
	if len([]rune(password)) < 10 {
		return fmt.Errorf("password must be at least 10 characters")
	}
	if len(password) > 72 {
		return fmt.Errorf("password must be at most 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	res, err := s.W.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoUser
	}
	return nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var created int64
	var lastLogin sql.NullInt64

	err := row.Scan(&u.ID, &u.Username, &u.StorageUsed, &u.StorageQuota, &created, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}

	u.CreatedAt = time.Unix(created, 0).UTC()
	if lastLogin.Valid {
		t := time.Unix(lastLogin.Int64, 0).UTC()
		u.LastLoginAt = &t
	}
	return &u, nil
}

// A valid bcrypt hash of a value nothing will ever match. Used purely to keep
// the failed-login path the same cost as the successful one.
const dummyHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEe.mHiJZ0zpM4l2zqXqAOWWxRz2mQ0Sxsy"
