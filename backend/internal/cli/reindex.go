package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"cloudapp/internal/b2"
	"cloudapp/internal/config"
	"cloudapp/internal/store"
)

// Reindex rebuilds the file table from the recovery sidecars in storage.
//
// This is the second line of defence in spec §7.3. The database is the only
// irreplaceable component — B2 holds every byte, but without SQLite there are
// no filenames, no folders and no dates. If the VPS and every nightly backup
// are lost, listing the meta/ prefix puts the library back.
//
// Written now rather than when it is needed: an untested recovery procedure is
// not a recovery procedure.
func Reindex(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	fromB2 := fs.Bool("from-b2", false, "rebuild from the sidecars in storage (required)")
	username := fs.String("username", "", "account to rebuild into (required)")
	dryRun := fs.Bool("dry-run", false, "report what would be restored without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*fromB2 {
		fs.Usage()
		return errors.New("--from-b2 is required; there is no other source to rebuild from")
	}
	if *username == "" {
		fs.Usage()
		return errors.New("--username is required")
	}

	storage := b2.NewService(cfg, st, log, false)
	if !storage.Configured() {
		return errors.New("B2 is not configured, so there is nothing to rebuild from")
	}

	var userID int64
	if err := st.R.QueryRowContext(ctx,
		`SELECT id FROM users WHERE username = ?`, strings.ToLower(*username)).Scan(&userID); err != nil {
		return fmt.Errorf("user %q not found — create it first with `cloudapp createuser`", *username)
	}

	prefix := fmt.Sprintf("users/%d/meta/", userID)
	fmt.Printf("Scanning %s\n", prefix)

	var scanned, restored, skipped int
	cursor := ""
	for {
		names, next, err := storage.List(ctx, prefix, cursor, 500)
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, object := range names {
			if !strings.HasSuffix(object.FileName, ".json") {
				continue
			}
			scanned++

			body, err := storage.GetObject(ctx, object.FileName)
			if err != nil {
				fmt.Printf("  skip %s: %v\n", object.FileName, err)
				skipped++
				continue
			}

			file, err := sidecarToFile(userID, body)
			if err != nil {
				fmt.Printf("  skip %s: %v\n", object.FileName, err)
				skipped++
				continue
			}

			if *dryRun {
				fmt.Printf("  would restore %s (%d bytes)\n", file.Filename, file.FileSize)
				restored++
				continue
			}
			if err := st.ReindexFile(ctx, file); err != nil {
				return fmt.Errorf("restore %s: %w", file.ObjectKey, err)
			}
			restored++
		}

		if next == "" {
			break
		}
		cursor = next
	}

	if *dryRun {
		fmt.Printf("\nDry run: %d sidecars scanned, %d would be restored, %d skipped.\n",
			scanned, restored, skipped)
		return nil
	}

	if err := st.RecomputeStorageUsed(ctx, userID); err != nil {
		return fmt.Errorf("recompute storage_used: %w", err)
	}

	fmt.Printf("\nRestored %d of %d files (%d skipped).\n", restored, scanned, skipped)
	fmt.Println("Thumbnails are marked unsupported so the background worker regenerates them.")
	fmt.Println("Folders are not recoverable from sidecars; everything landed in the root.")
	return nil
}

// sidecar mirrors what the upload handler writes next to each object.
type sidecar struct {
	ObjectKey   string   `json:"object_key"`
	Filename    string   `json:"filename"`
	Ext         string   `json:"ext"`
	MimeType    string   `json:"mime_type"`
	Kind        string   `json:"kind"`
	FileSize    int64    `json:"file_size"`
	Sha1        string   `json:"sha1"`
	B2FileID    string   `json:"b2_file_id"`
	Width       *int     `json:"width"`
	Height      *int     `json:"height"`
	DurationSec *float64 `json:"duration_sec"`
	TakenAt     int64    `json:"taken_at"`
	UploadedAt  int64    `json:"uploaded_at"`
}

func sidecarToFile(userID int64, body []byte) (*store.File, error) {
	var s sidecar
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("unreadable sidecar: %w", err)
	}
	if s.ObjectKey == "" || s.Filename == "" {
		return nil, errors.New("sidecar is missing its object key or filename")
	}
	if s.Kind != "image" && s.Kind != "video" {
		return nil, fmt.Errorf("unexpected kind %q", s.Kind)
	}
	if s.TakenAt <= 0 {
		s.TakenAt = s.UploadedAt
	}

	return &store.File{
		UserID:      userID,
		ObjectKey:   s.ObjectKey,
		Filename:    s.Filename,
		Ext:         s.Ext,
		MimeType:    s.MimeType,
		Kind:        s.Kind,
		FileSize:    s.FileSize,
		Sha1:        s.Sha1,
		B2FileID:    s.B2FileID,
		Width:       s.Width,
		Height:      s.Height,
		DurationSec: s.DurationSec,
		TakenAt:     s.TakenAt,
		UploadedAt:  s.UploadedAt,
	}, nil
}
