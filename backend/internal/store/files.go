package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxFileSize is the per-file ceiling from spec §5.
const MaxFileSize = 500 << 20

var (
	ErrNoFile        = errors.New("file not found")
	ErrQuotaExceeded = errors.New("storage quota exceeded")
	ErrWrongState    = errors.New("file is not in the expected state")
	ErrBadCursor     = errors.New("malformed cursor")
)

type File struct {
	ID        int64
	UserID    int64
	FolderID  *int64
	ObjectKey string
	Filename  string
	Ext       string
	MimeType  string
	Kind      string
	FileSize  int64
	Sha1      string

	B2FileID      string
	HasThumb      bool
	ThumbStatus   string
	ThumbTier     string
	ThumbFormat   string
	ThumbAttempts int

	Width       *int
	Height      *int
	DurationSec *float64
	TakenAt     int64

	Status     string
	UploadedAt int64
	IsDeleted  bool
	DeletedAt  *int64
}

// NewFile is what a client declares at the start of an upload. None of it is
// trusted until Complete verifies the object against B2 (spec §4.2).
type NewFile struct {
	UserID   int64
	FolderID *int64
	Filename string
	MimeType string
	Kind     string
	Size     int64
	Sha1     string
	TakenAt  int64
}

// CompleteFile is what the client reports once the bytes are in B2.
type CompleteFile struct {
	B2FileID    string
	Sha1        string
	Width       *int
	Height      *int
	DurationSec *float64
	TakenAt     int64
	ThumbOK     bool
	ThumbTier   string
	ThumbFormat string
}

const fileColumns = `id, user_id, folder_id, object_key, filename, ext, mime_type, kind,
	file_size, sha1, b2_file_id, has_thumb, thumb_status, thumb_tier, thumb_format,
	thumb_attempts, width, height, duration_sec, taken_at, status, uploaded_at,
	is_deleted, deleted_at`

// ---------------------------------------------------------------- create

// BeginUpload reserves a row and an object key.
//
// The quota check and the insert share one transaction, and the check counts
// uploads already in flight as well as bytes already stored. Two parallel
// inits that would each fit on their own but not together must not both pass —
// with the writer pinned to a single connection and _txlock=immediate, they
// cannot.
func (s *Store) BeginUpload(ctx context.Context, n NewFile) (*File, error) {
	if n.Size <= 0 {
		return nil, fmt.Errorf("file size must be positive")
	}
	if n.Size > MaxFileSize {
		return nil, fmt.Errorf("file is %d bytes; the limit is %d", n.Size, int64(MaxFileSize))
	}

	objectKey, err := randomKey()
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	takenAt := n.TakenAt
	// Gallery order is by taken_at, so it must never be null: a photo library
	// sorted by upload time is useless after the first bulk import, and a NULL
	// here would silently drop the row out of the keyset pagination.
	if takenAt <= 0 {
		takenAt = now
	}

	tx, err := s.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var quota, used, inflight int64
	err = tx.QueryRowContext(ctx, `
		SELECT storage_quota, storage_used,
		       COALESCE((SELECT SUM(file_size) FROM files
		                 WHERE user_id = users.id AND status = 'uploading'), 0)
		FROM users WHERE id = ?`, n.UserID).Scan(&quota, &used, &inflight)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	if used+inflight+n.Size > quota {
		return nil, fmt.Errorf("%w: %d bytes stored, %d in flight, %d requested, quota %d",
			ErrQuotaExceeded, used, inflight, n.Size, quota)
	}

	ext := extensionOf(n.Filename)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO files (user_id, folder_id, object_key, filename, ext, mime_type, kind,
		                   file_size, sha1, taken_at, status, uploaded_at, thumb_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'uploading', ?, 'pending')`,
		n.UserID, n.FolderID, objectKey, n.Filename, ext, n.MimeType, n.Kind,
		n.Size, nullString(n.Sha1), takenAt, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.FileByID(ctx, n.UserID, id)
}

// CompleteUpload flips a row to ready and charges the bytes against the quota,
// in one transaction. Splitting them would let a crash leave storage that is
// billed by B2 but invisible to the accounting.
func (s *Store) CompleteUpload(ctx context.Context, userID, fileID int64, c CompleteFile) (*File, error) {
	tx, err := s.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status string
	var size int64
	err = tx.QueryRowContext(ctx,
		`SELECT status, file_size FROM files WHERE id = ? AND user_id = ?`, fileID, userID).
		Scan(&status, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoFile
	}
	if err != nil {
		return nil, err
	}
	if status != "uploading" {
		return nil, fmt.Errorf("%w: file is %q, expected uploading", ErrWrongState, status)
	}

	thumbStatus := "ready"
	if !c.ThumbOK {
		// The client could not build a thumbnail. The original is stored
		// regardless; this hands the row to the tier 3 worker (spec §5.4).
		thumbStatus = "unsupported"
	}

	takenAt := c.TakenAt
	if takenAt <= 0 {
		if err := tx.QueryRowContext(ctx, `SELECT uploaded_at FROM files WHERE id = ?`, fileID).
			Scan(&takenAt); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE files SET status = 'ready', b2_file_id = ?, sha1 = COALESCE(?, sha1),
		       width = ?, height = ?, duration_sec = ?, taken_at = ?,
		       has_thumb = ?, thumb_status = ?, thumb_tier = ?, thumb_format = ?
		WHERE id = ? AND user_id = ? AND status = 'uploading'`,
		c.B2FileID, nullString(c.Sha1), c.Width, c.Height, c.DurationSec, takenAt,
		boolInt(c.ThumbOK), thumbStatus, nullString(c.ThumbTier), nullString(c.ThumbFormat),
		fileID, userID); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET storage_used = storage_used + ? WHERE id = ?`, size, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.FileByID(ctx, userID, fileID)
}

// AbandonUpload drops a row that never completed. The bytes were never charged
// against the quota, so nothing needs unwinding.
func (s *Store) AbandonUpload(ctx context.Context, userID, fileID int64) error {
	res, err := s.W.ExecContext(ctx,
		`DELETE FROM files WHERE id = ? AND user_id = ? AND status = 'uploading'`, fileID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoFile
	}
	return nil
}

// FindDuplicate returns an existing ready file with the same content hash.
// B2 requires a SHA-1 on every upload anyway, so deduplication costs nothing.
func (s *Store) FindDuplicate(ctx context.Context, userID int64, sha1 string) (*File, error) {
	if sha1 == "" {
		return nil, nil
	}
	f, err := s.scanFile(s.R.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE user_id = ? AND sha1 = ? AND status = 'ready' AND is_deleted = 0
		 LIMIT 1`, userID, sha1))
	if errors.Is(err, ErrNoFile) {
		return nil, nil
	}
	return f, err
}

// ---------------------------------------------------------------- read

func (s *Store) FileByID(ctx context.Context, userID, id int64) (*File, error) {
	return s.scanFile(s.R.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE id = ? AND user_id = ?`, id, userID))
}

type ListFilter struct {
	FolderID   *int64
	InFolder   bool // distinguishes "root only" from "no folder filter"
	Kind       string
	Query      string
	Deleted    bool
	Cursor     string
	Limit      int
	IncludeAll bool // ignore the ready filter; used by maintenance jobs
}

type ListResult struct {
	Files      []*File
	NextCursor string
}

// ListFiles pages with a keyset on (taken_at, id).
//
// Not OFFSET: a photo library is exactly the workload where offset pagination
// degrades, since page N must walk N*limit rows and shifts under concurrent
// inserts.
func (s *Store) ListFiles(ctx context.Context, userID int64, f ListFilter) (*ListResult, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	where := []string{"user_id = ?"}
	args := []any{userID}

	if f.Deleted {
		where = append(where, "is_deleted = 1")
	} else {
		where = append(where, "is_deleted = 0")
	}
	if !f.IncludeAll {
		where = append(where, "status = 'ready'")
	}
	if f.InFolder {
		if f.FolderID == nil {
			where = append(where, "folder_id IS NULL")
		} else {
			where = append(where, "folder_id = ?")
			args = append(args, *f.FolderID)
		}
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// LIKE is correct at this scale; an FTS5 table would be premature for
		// a single user's filenames.
		where = append(where, "filename LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(q)+"%")
	}
	if f.Cursor != "" {
		takenAt, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
		where = append(where, "(taken_at < ? OR (taken_at = ? AND id < ?))")
		args = append(args, takenAt, takenAt, id)
	}

	// One extra row tells us whether another page exists without a COUNT.
	args = append(args, limit+1)
	rows, err := s.R.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM files WHERE `+strings.Join(where, " AND ")+
			` ORDER BY taken_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &ListResult{}
	for rows.Next() {
		f, err := scanFileRow(rows)
		if err != nil {
			return nil, err
		}
		out.Files = append(out.Files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(out.Files) > limit {
		last := out.Files[limit-1]
		out.Files = out.Files[:limit]
		out.NextCursor = encodeCursor(last.TakenAt, last.ID)
	}
	return out, nil
}

// StorageStats powers the usage endpoint and the sidebar meter.
type StorageStats struct {
	Used    int64 `json:"used_bytes"`
	Quota   int64 `json:"quota_bytes"`
	Images  int64 `json:"image_bytes"`
	Videos  int64 `json:"video_bytes"`
	Trash   int64 `json:"trash_bytes"`
	Count   int64 `json:"file_count"`
	Pending int64 `json:"thumbnails_pending"`
}

func (s *Store) StorageStats(ctx context.Context, userID int64) (*StorageStats, error) {
	var st StorageStats
	err := s.R.QueryRowContext(ctx,
		`SELECT storage_used, storage_quota FROM users WHERE id = ?`, userID).
		Scan(&st.Used, &st.Quota)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}

	err = s.R.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN kind = 'image' AND is_deleted = 0 THEN file_size END), 0),
		  COALESCE(SUM(CASE WHEN kind = 'video' AND is_deleted = 0 THEN file_size END), 0),
		  COALESCE(SUM(CASE WHEN is_deleted = 1 THEN file_size END), 0),
		  COALESCE(SUM(CASE WHEN is_deleted = 0 THEN 1 END), 0),
		  COALESCE(SUM(CASE WHEN is_deleted = 0 AND thumb_status = 'unsupported' THEN 1 END), 0)
		FROM files WHERE user_id = ? AND status = 'ready'`, userID).
		Scan(&st.Images, &st.Videos, &st.Trash, &st.Count, &st.Pending)
	return &st, err
}

// ---------------------------------------------------------------- mutate

func (s *Store) RenameFile(ctx context.Context, userID, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 255 {
		return fmt.Errorf("filename must be 1-255 characters")
	}
	res, err := s.W.ExecContext(ctx,
		`UPDATE files SET filename = ? WHERE id = ? AND user_id = ? AND is_deleted = 0`,
		name, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoFile
	}
	return nil
}

// MoveFile reparents a file. folderID nil means the root.
func (s *Store) MoveFile(ctx context.Context, userID, id int64, folderID *int64) error {
	if folderID != nil {
		var exists int
		if err := s.R.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM folders WHERE id = ? AND user_id = ?`, *folderID, userID).
			Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNoFolder
		}
	}
	res, err := s.W.ExecContext(ctx,
		`UPDATE files SET folder_id = ? WHERE id = ? AND user_id = ? AND is_deleted = 0`,
		folderID, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoFile
	}
	return nil
}

// SoftDelete moves files to the trash and releases their quota in the same
// transaction. The B2 objects survive for thirty days; the purge job in spec
// §7.2 is what finally stops the billing.
func (s *Store) SoftDelete(ctx context.Context, userID int64, ids []int64) (int64, error) {
	return s.setDeleted(ctx, userID, ids, true)
}

func (s *Store) Restore(ctx context.Context, userID int64, ids []int64) (int64, error) {
	return s.setDeleted(ctx, userID, ids, false)
}

func (s *Store) setDeleted(ctx context.Context, userID int64, ids []int64, deleted bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := s.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	placeholders := make([]string, len(ids))
	args := []any{userID}
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	in := strings.Join(placeholders, ",")

	from := 0
	if !deleted {
		from = 1
	}

	// Sum before updating, and only over rows that will actually change, so a
	// double delete cannot subtract the same bytes twice.
	var bytes int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(file_size), 0) FROM files
		 WHERE user_id = ? AND id IN (`+in+`) AND is_deleted = `+strconv.Itoa(from)+
			` AND status = 'ready'`, args...).Scan(&bytes); err != nil {
		return 0, err
	}

	var res sql.Result
	if deleted {
		res, err = tx.ExecContext(ctx,
			`UPDATE files SET is_deleted = 1, deleted_at = unixepoch()
			 WHERE user_id = ? AND id IN (`+in+`) AND is_deleted = 0`, args...)
	} else {
		res, err = tx.ExecContext(ctx,
			`UPDATE files SET is_deleted = 0, deleted_at = NULL
			 WHERE user_id = ? AND id IN (`+in+`) AND is_deleted = 1`, args...)
	}
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()

	if bytes != 0 {
		delta := -bytes
		if !deleted {
			delta = bytes
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET storage_used = MAX(0, storage_used + ?) WHERE id = ?`,
			delta, userID); err != nil {
			return 0, err
		}
	}
	return affected, tx.Commit()
}

// ---------------------------------------------------------------- maintenance

// PurgeCandidates lists rows whose B2 objects are due for permanent deletion.
func (s *Store) PurgeCandidates(ctx context.Context, olderThan time.Duration, limit int) ([]*File, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	rows, err := s.R.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE is_deleted = 1 AND deleted_at IS NOT NULL AND deleted_at < ?
		 LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFiles(rows)
}

// OrphanCandidates lists uploads that never completed.
func (s *Store) OrphanCandidates(ctx context.Context, olderThan time.Duration, limit int) ([]*File, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	rows, err := s.R.QueryContext(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE status = 'uploading' AND uploaded_at < ? LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFiles(rows)
}

// HardDelete removes the row once its B2 objects are gone.
func (s *Store) HardDelete(ctx context.Context, id int64) error {
	_, err := s.W.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	return err
}

// ReconcileStorageUsed recomputes the cached counter from the rows themselves.
// Any drift means a bug in the transaction boundaries, so it is reported rather
// than silently corrected.
func (s *Store) ReconcileStorageUsed(ctx context.Context, userID int64) (cached, actual int64, err error) {
	err = s.R.QueryRowContext(ctx, `
		SELECT u.storage_used,
		       COALESCE((SELECT SUM(file_size) FROM files
		                 WHERE user_id = u.id AND is_deleted = 0 AND status = 'ready'), 0)
		FROM users u WHERE u.id = ?`, userID).Scan(&cached, &actual)
	return
}

// ---------------------------------------------------------------- scanning

func (s *Store) scanFile(row *sql.Row) (*File, error) {
	f, err := scanFileInto(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoFile
	}
	return f, err
}

func scanFileRow(rows *sql.Rows) (*File, error) {
	return scanFileInto(rows.Scan)
}

func scanFileInto(scan func(...any) error) (*File, error) {
	var f File
	var folderID, width, height, deletedAt sql.NullInt64
	var duration sql.NullFloat64
	var sha1, b2ID, thumbTier, thumbFormat sql.NullString
	var hasThumb, isDeleted int

	if err := scan(
		&f.ID, &f.UserID, &folderID, &f.ObjectKey, &f.Filename, &f.Ext, &f.MimeType, &f.Kind,
		&f.FileSize, &sha1, &b2ID, &hasThumb, &f.ThumbStatus, &thumbTier, &thumbFormat,
		&f.ThumbAttempts, &width, &height, &duration, &f.TakenAt, &f.Status, &f.UploadedAt,
		&isDeleted, &deletedAt,
	); err != nil {
		return nil, err
	}

	f.HasThumb = hasThumb != 0
	f.IsDeleted = isDeleted != 0
	f.Sha1, f.B2FileID, f.ThumbTier, f.ThumbFormat = sha1.String, b2ID.String, thumbTier.String, thumbFormat.String
	if folderID.Valid {
		f.FolderID = &folderID.Int64
	}
	if width.Valid {
		v := int(width.Int64)
		f.Width = &v
	}
	if height.Valid {
		v := int(height.Int64)
		f.Height = &v
	}
	if duration.Valid {
		f.DurationSec = &duration.Float64
	}
	if deletedAt.Valid {
		f.DeletedAt = &deletedAt.Int64
	}
	return &f, nil
}

func collectFiles(rows *sql.Rows) ([]*File, error) {
	var out []*File
	for rows.Next() {
		f, err := scanFileRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- helpers

func randomKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func encodeCursor(takenAt, id int64) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(takenAt, 10) + "." + strconv.FormatInt(id, 10)))
}

func decodeCursor(c string) (takenAt, id int64, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, 0, ErrBadCursor
	}
	parts := strings.SplitN(string(raw), ".", 2)
	if len(parts) != 2 {
		return 0, 0, ErrBadCursor
	}
	if takenAt, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
		return 0, 0, ErrBadCursor
	}
	if id, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return 0, 0, ErrBadCursor
	}
	return takenAt, id, nil
}

// escapeLike neutralises the wildcards so a search for "100%" does not match
// everything.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func extensionOf(filename string) string {
	if i := strings.LastIndex(filename, "."); i >= 0 && i < len(filename)-1 {
		ext := strings.ToLower(filename[i+1:])
		if len(ext) > 11 {
			ext = ext[:11]
		}
		return ext
	}
	return ""
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- thumbnail jobs

// ClaimThumbnailJob hands out one file for the tier 3 worker (spec §5.4).
//
// The attempt counter is incremented in the same statement that selects the
// row, so a crash mid-job costs one attempt rather than looping forever on a
// file ffmpeg cannot handle.
func (s *Store) ClaimThumbnailJob(ctx context.Context, maxAttempts int) (*File, error) {
	tx, err := s.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM files
		WHERE thumb_status = 'unsupported' AND thumb_attempts < ?
		  AND is_deleted = 0 AND status = 'ready'
		ORDER BY thumb_attempts, uploaded_at
		LIMIT 1`, maxAttempts).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE files SET thumb_attempts = thumb_attempts + 1 WHERE id = ?`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	var userID int64
	if err := s.R.QueryRowContext(ctx, `SELECT user_id FROM files WHERE id = ?`, id).Scan(&userID); err != nil {
		return nil, err
	}
	return s.FileByID(ctx, userID, id)
}

// MarkThumbnailReady records a thumbnail the server produced.
func (s *Store) MarkThumbnailReady(ctx context.Context, id int64, tier, format string) error {
	_, err := s.W.ExecContext(ctx,
		`UPDATE files SET thumb_status = 'ready', has_thumb = 1, thumb_tier = ?, thumb_format = ?
		 WHERE id = ?`, tier, format, id)
	return err
}

// MarkThumbnailFailed gives up on a file for good, so the worker stops
// re-reading it every two minutes forever.
func (s *Store) MarkThumbnailFailed(ctx context.Context, id int64) error {
	_, err := s.W.ExecContext(ctx, `UPDATE files SET thumb_status = 'failed' WHERE id = ?`, id)
	return err
}

// AllUserIDs is used by the jobs that run per user.
func (s *Store) AllUserIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.R.QueryContext(ctx, `SELECT id FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- backup

// VacuumInto writes a consistent copy of the database to path, which must not
// already exist. This is the whole of the backup strategy in spec §7.3: the
// bytes live in B2 regardless, but without this file there are no filenames,
// no folders and no dates.
func (s *Store) VacuumInto(ctx context.Context, path string) error {
	_, err := s.W.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// ReindexFile restores one row from a recovery sidecar. Idempotent on
// object_key so a rerun over the same prefix does not duplicate anything.
func (s *Store) ReindexFile(ctx context.Context, f *File) error {
	_, err := s.W.ExecContext(ctx, `
		INSERT INTO files (user_id, object_key, filename, ext, mime_type, kind, file_size,
		                   sha1, b2_file_id, width, height, duration_sec, taken_at,
		                   status, uploaded_at, thumb_status, has_thumb)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', ?, 'unsupported', 0)
		ON CONFLICT(object_key) DO NOTHING`,
		f.UserID, f.ObjectKey, f.Filename, f.Ext, f.MimeType, f.Kind, f.FileSize,
		nullString(f.Sha1), nullString(f.B2FileID), f.Width, f.Height, f.DurationSec,
		f.TakenAt, f.UploadedAt)
	return err
}

// RecomputeStorageUsed rewrites the cached counter from the rows themselves.
// Only the reindex path uses it: everywhere else a drift is a bug to be
// reported, not silently papered over.
func (s *Store) RecomputeStorageUsed(ctx context.Context, userID int64) error {
	_, err := s.W.ExecContext(ctx, `
		UPDATE users SET storage_used = COALESCE(
			(SELECT SUM(file_size) FROM files
			 WHERE user_id = ? AND is_deleted = 0 AND status = 'ready'), 0)
		WHERE id = ?`, userID, userID)
	return err
}
