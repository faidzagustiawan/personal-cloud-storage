package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNoFolder       = errors.New("folder not found")
	ErrFolderExists   = errors.New("a folder with that name already exists")
	ErrFolderNotEmpty = errors.New("folder is not empty")
)

// Folders are flat in v1 (decision D5). parent_folder_id exists in the schema
// and is always NULL, so nesting becomes a feature change rather than a
// migration.
type Folder struct {
	ID        int64
	UserID    int64
	Name      string
	CreatedAt int64
	FileCount int64
}

func (s *Store) CreateFolder(ctx context.Context, userID int64, name string) (*Folder, error) {
	name = strings.TrimSpace(name)
	if err := validFolderName(name); err != nil {
		return nil, err
	}

	res, err := s.W.ExecContext(ctx,
		`INSERT INTO folders (user_id, name, parent_folder_id, created_at)
		 VALUES (?, ?, NULL, ?)`, userID, name, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrFolderExists
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.FolderByID(ctx, userID, id)
}

func (s *Store) FolderByID(ctx context.Context, userID, id int64) (*Folder, error) {
	var f Folder
	err := s.R.QueryRowContext(ctx, `
		SELECT f.id, f.user_id, f.name, f.created_at,
		       COALESCE((SELECT COUNT(*) FROM files
		                 WHERE folder_id = f.id AND is_deleted = 0 AND status = 'ready'), 0)
		FROM folders f WHERE f.id = ? AND f.user_id = ?`, id, userID).
		Scan(&f.ID, &f.UserID, &f.Name, &f.CreatedAt, &f.FileCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoFolder
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *Store) ListFolders(ctx context.Context, userID int64) ([]*Folder, error) {
	rows, err := s.R.QueryContext(ctx, `
		SELECT f.id, f.user_id, f.name, f.created_at,
		       COALESCE((SELECT COUNT(*) FROM files
		                 WHERE folder_id = f.id AND is_deleted = 0 AND status = 'ready'), 0)
		FROM folders f WHERE f.user_id = ? ORDER BY f.name COLLATE NOCASE`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.UserID, &f.Name, &f.CreatedAt, &f.FileCount); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

func (s *Store) RenameFolder(ctx context.Context, userID, id int64, name string) error {
	name = strings.TrimSpace(name)
	if err := validFolderName(name); err != nil {
		return err
	}
	res, err := s.W.ExecContext(ctx,
		`UPDATE folders SET name = ? WHERE id = ? AND user_id = ?`, name, id, userID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrFolderExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoFolder
	}
	return nil
}

// DeleteFolderMode says what to do with the files a folder still holds.
type DeleteFolderMode string

const (
	// DeleteRefuse is the default: a non-empty folder is not deleted silently.
	DeleteRefuse DeleteFolderMode = "refuse"
	// DeleteMoveToRoot keeps the files and empties the folder.
	DeleteMoveToRoot DeleteFolderMode = "move_to_root"
	// DeleteCascade soft-deletes the files, so they stay recoverable for
	// thirty days rather than vanishing.
	DeleteCascade DeleteFolderMode = "cascade"
)

func (s *Store) DeleteFolder(ctx context.Context, userID, id int64, mode DeleteFolderMode) (int64, error) {
	tx, err := s.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var owned int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM folders WHERE id = ? AND user_id = ?`, id, userID).Scan(&owned); err != nil {
		return 0, err
	}
	if owned == 0 {
		return 0, ErrNoFolder
	}

	var contained, containedBytes int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(file_size), 0) FROM files
		 WHERE folder_id = ? AND user_id = ? AND is_deleted = 0 AND status = 'ready'`,
		id, userID).Scan(&contained, &containedBytes); err != nil {
		return 0, err
	}

	var affected int64
	switch {
	case contained == 0:
		// Nothing to decide.
	case mode == DeleteMoveToRoot:
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET folder_id = NULL WHERE folder_id = ? AND user_id = ?`, id, userID)
		if err != nil {
			return 0, err
		}
		affected, _ = res.RowsAffected()
	case mode == DeleteCascade:
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET is_deleted = 1, deleted_at = unixepoch()
			 WHERE folder_id = ? AND user_id = ? AND is_deleted = 0`, id, userID)
		if err != nil {
			return 0, err
		}
		affected, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET storage_used = MAX(0, storage_used - ?) WHERE id = ?`,
			containedBytes, userID); err != nil {
			return 0, err
		}
	default:
		return contained, ErrFolderNotEmpty
	}

	// Any remaining rows are already-trashed files still pointing here; the
	// foreign key would block the delete, so cut them loose first.
	if _, err := tx.ExecContext(ctx,
		`UPDATE files SET folder_id = NULL WHERE folder_id = ? AND user_id = ?`, id, userID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM folders WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return 0, err
	}
	return affected, tx.Commit()
}

func validFolderName(name string) error {
	if name == "" {
		return fmt.Errorf("folder name cannot be empty")
	}
	if len([]rune(name)) > 64 {
		return fmt.Errorf("folder name must be at most 64 characters")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("folder name cannot contain slashes")
	}
	return nil
}
