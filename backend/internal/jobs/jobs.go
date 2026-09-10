// Package jobs runs the background maintenance of spec §7.2.
//
// One goroutine, one ticker, and every job idempotent and resumable from what
// is in the database. There is no in-memory queue and nothing is scheduled in
// process memory, so a restart mid-job costs at most one repetition rather than
// losing work — which is the whole reason the purge and the thumbnail worker
// claim their rows from SQLite instead of a channel.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloudapp/internal/b2"
	"cloudapp/internal/store"
)

const (
	// TrashRetention is how long a deleted file stays recoverable before its
	// storage is released for good (spec §7.2).
	TrashRetention = 30 * 24 * time.Hour
	// OrphanAge is how long an upload may sit unfinished before it is swept.
	OrphanAge = 24 * time.Hour
	// BackupRetention is how many nightly database copies are kept in storage.
	BackupRetention = 14

	tickInterval  = time.Minute
	thumbInterval = 2 * time.Minute
	hourly        = time.Hour
	daily         = 24 * time.Hour
	weekly        = 7 * 24 * time.Hour

	maxThumbAttempts = 3
	purgeBatch       = 100
)

type Runner struct {
	Store *store.Store
	B2    *b2.Service
	Log   *slog.Logger
	// TempDir is where the nightly VACUUM INTO copy is written before upload.
	TempDir string

	thumbs *thumbnailWorker
	last   map[string]time.Time
}

func New(st *store.Store, storage *b2.Service, log *slog.Logger, tempDir string) *Runner {
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return &Runner{
		Store:   st,
		B2:      storage,
		Log:     log,
		TempDir: tempDir,
		thumbs:  newThumbnailWorker(st, storage, log),
		last:    map[string]time.Time{},
	}
}

// Run ticks until the context is cancelled.
//
// The ticker is deliberately short and each job decides for itself whether it is
// due. That way a server restarted at 03:05 still runs the nightly jobs that day
// instead of waiting until tomorrow.
func (r *Runner) Run(ctx context.Context) {
	r.Log.Info("background jobs started",
		"thumbnails", r.thumbs.available(),
		"trash_retention_days", int(TrashRetention.Hours()/24))

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// A first pass immediately: a server that has been down for a week has work
	// waiting, and B2 has been billing for it the whole time.
	r.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			r.Log.Info("background jobs stopped")
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	r.every(ctx, "thumbnails", thumbInterval, r.runThumbnails)
	r.every(ctx, "orphans", hourly, r.sweepOrphans)
	r.every(ctx, "tokens", hourly, r.refreshTokens)
	r.every(ctx, "purge", daily, r.purge)
	r.every(ctx, "backup", daily, r.backup)
	r.every(ctx, "reconcile", weekly, r.reconcile)
}

func (r *Runner) every(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	if ctx.Err() != nil {
		return
	}
	if last, ok := r.last[name]; ok && time.Since(last) < interval {
		return
	}
	r.last[name] = time.Now()

	if err := fn(ctx); err != nil && ctx.Err() == nil {
		// A failed job is logged and retried on its next turn. Nothing here is
		// worth taking the server down for.
		r.Log.Error("job failed", "job", name, "err", err)
	}
}

// ---------------------------------------------------------------- jobs

func (r *Runner) runThumbnails(ctx context.Context) error {
	return r.thumbs.runOne(ctx)
}

// sweepOrphans removes uploads that never completed.
//
// B2 bills the uploaded parts of an unfinished large file for as long as it
// stays unfinished, and the reserved row counts against the quota, so leaving
// these costs both money and space.
func (r *Runner) sweepOrphans(ctx context.Context) error {
	if !r.B2.Configured() {
		return nil
	}

	files, err := r.Store.OrphanCandidates(ctx, OrphanAge, purgeBatch)
	if err != nil {
		return err
	}
	for _, file := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if file.B2FileID != "" {
			name := b2.OrigName(file.UserID, file.ObjectKey, file.Ext)
			if err := r.B2.Delete(ctx, name, file.B2FileID); err != nil {
				r.Log.Warn("could not delete orphan object", "object_key", file.ObjectKey, "err", err)
				continue
			}
		}
		if err := r.Store.HardDelete(ctx, file.ID); err != nil {
			return err
		}
	}
	if len(files) > 0 {
		r.Log.Info("orphan sweep", "removed", len(files))
	}
	return nil
}

// purge is what actually stops the billing.
//
// A soft-deleted row whose storage object is never removed is paid for forever;
// this is the single most expensive thing that can be left out.
func (r *Runner) purge(ctx context.Context) error {
	if !r.B2.Configured() {
		return nil
	}

	files, err := r.Store.PurgeCandidates(ctx, TrashRetention, purgeBatch)
	if err != nil {
		return err
	}

	var removed int
	for _, file := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Order matters: storage first, row last. A crash between the two leaves
		// a row whose object is already gone, and the next pass deletes it
		// harmlessly. The reverse would orphan the object with nothing left
		// pointing at it — invisible, and billed forever.
		orig := b2.OrigName(file.UserID, file.ObjectKey, file.Ext)
		if err := r.B2.Delete(ctx, orig, file.B2FileID); err != nil {
			r.Log.Warn("could not purge original", "object_key", file.ObjectKey, "err", err)
			continue
		}
		// The thumbnail and the sidecar have no stored file id, so they are
		// removed by name; a miss is not an error.
		r.deleteByName(ctx, b2.ThumbName(file.UserID, file.ObjectKey))
		r.deleteByName(ctx, b2.MetaName(file.UserID, file.ObjectKey))

		if err := r.Store.HardDelete(ctx, file.ID); err != nil {
			return err
		}
		removed++
	}

	if _, err := r.Store.PurgeExpiredSessions(ctx); err != nil {
		r.Log.Warn("session purge failed", "err", err)
	}
	if _, err := r.Store.PurgeDeadShares(ctx); err != nil {
		r.Log.Warn("share purge failed", "err", err)
	}

	if removed > 0 {
		r.Log.Info("purged trashed files", "count", removed)
	}
	return nil
}

// deleteByName removes an object whose stored file id we never kept.
func (r *Runner) deleteByName(ctx context.Context, name string) {
	found, _, err := r.B2.List(ctx, name, "", 1)
	if err != nil || len(found) == 0 || found[0].FileName != name {
		return
	}
	if err := r.B2.Delete(ctx, name, found[0].FileID); err != nil {
		r.Log.Warn("could not delete object", "name", name, "err", err)
	}
}

// refreshTokens renews the prefix download authorization before it lapses, so
// no page load ever races an expiry.
func (r *Runner) refreshTokens(ctx context.Context) error {
	if !r.B2.Configured() {
		return nil
	}
	users, err := r.Store.AllUserIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range users {
		// Composing a URL is what mints or reuses the cached token, so this is
		// both the check and the refresh.
		if _, err := r.B2.DownloadURL(ctx, id, "warmup"); err != nil {
			r.Log.Warn("could not refresh download token", "user_id", id, "err", err)
		}
	}
	return nil
}

// backup copies the database to storage.
//
// The bytes are safe in B2 regardless, but without this file there are no
// filenames, no folders and no dates — so this and the per-file sidecars are
// the two things standing between a lost VPS and a lost library (spec §7.3).
func (r *Runner) backup(ctx context.Context) error {
	if !r.B2.Configured() {
		return nil
	}

	users, err := r.Store.AllUserIDs(ctx)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	owner := users[0]
	day := time.Now().UTC().Format("20060102")

	// The scheduler tracks its last run in memory, so a restart makes every
	// daily job due again. Asking storage whether today's copy already exists
	// makes this idempotent per day rather than per process — a server
	// restarted twenty times in an afternoon otherwise uploads the database
	// twenty times, and B2 bills each superseded version until the lifecycle
	// rule clears it.
	name := b2.BackupName(owner, day)
	if existing, _, err := r.B2.List(ctx, name, "", 1); err == nil {
		for _, object := range existing {
			if object.FileName == name {
				return nil
			}
		}
	}

	path := filepath.Join(r.TempDir, fmt.Sprintf("cloud-backup-%s.sqlite", day))
	// VACUUM INTO refuses to overwrite, so a leftover from a failed run must go.
	_ = os.Remove(path)
	defer os.Remove(path)

	if err := r.Store.VacuumInto(ctx, path); err != nil {
		return fmt.Errorf("vacuum into %s: %w", path, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if err := r.B2.PutBackup(ctx, owner, day, body); err != nil {
		return err
	}
	r.Log.Info("database backed up", "day", day, "bytes", len(body))

	r.trimBackups(ctx, owner)
	return nil
}

// trimBackups keeps the most recent BackupRetention copies.
func (r *Runner) trimBackups(ctx context.Context, userID int64) {
	prefix := strings.TrimSuffix(b2.BackupName(userID, ""), "cloud-.sqlite")
	found, _, err := r.B2.List(ctx, prefix, "", 200)
	if err != nil {
		r.Log.Warn("could not list backups", "err", err)
		return
	}
	if len(found) <= BackupRetention {
		return
	}
	// B2 lists names in lexicographic order and the names are date-stamped, so
	// the oldest are at the front.
	for _, old := range found[:len(found)-BackupRetention] {
		if err := r.B2.Delete(ctx, old.FileName, old.FileID); err != nil {
			r.Log.Warn("could not delete old backup", "name", old.FileName, "err", err)
		}
	}
}

// reconcile compares the cached storage counter against the rows.
//
// Drift means a transaction boundary is wrong somewhere, so it is reported
// rather than silently corrected — quietly fixing it would hide the bug that
// caused it.
func (r *Runner) reconcile(ctx context.Context) error {
	users, err := r.Store.AllUserIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range users {
		cached, actual, err := r.Store.ReconcileStorageUsed(ctx, id)
		if err != nil {
			return err
		}
		if cached != actual {
			r.Log.Error("storage_used has drifted from the file rows",
				"user_id", id, "cached", cached, "actual", actual, "difference", cached-actual)
		}
	}
	return nil
}
