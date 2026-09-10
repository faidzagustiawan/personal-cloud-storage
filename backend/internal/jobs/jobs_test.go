package jobs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudapp/internal/b2"
	"cloudapp/internal/config"
	"cloudapp/internal/store"
)

// The purge job is the one that stops B2 charging for deleted files, and the
// backup is the only thing standing between a lost VPS and a library with no
// filenames. Both get tested against a fake bucket that tracks what it holds.

type fakeBucket struct {
	mu      sync.Mutex
	objects map[string]string // name -> fileId
	deleted []string
	srv     *httptest.Server
}

func newFakeBucket(t *testing.T) *fakeBucket {
	t.Helper()
	f := &fakeBucket{objects: map[string]string{}}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			json.Unmarshal(raw, &body)
		}

		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case endpoint == "b2_authorize_account":
			json.NewEncoder(w).Encode(map[string]any{
				"accountId": "a", "authorizationToken": "t",
				"apiUrl": f.srv.URL, "downloadUrl": f.srv.URL,
				"recommendedPartSize": 1 << 20, "absoluteMinimumPartSize": 1 << 20,
				"allowed": map[string]any{"capabilities": []string{"readFiles", "writeFiles", "deleteFiles", "listFiles"}},
			})

		case endpoint == "b2_delete_file_version":
			name, _ := body["fileName"].(string)
			if _, ok := f.objects[name]; !ok {
				json.NewEncoder(w).Encode(map[string]any{
					"status": 400, "code": "file_not_present", "message": "gone",
				})
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			delete(f.objects, name)
			f.deleted = append(f.deleted, name)
			json.NewEncoder(w).Encode(map[string]any{"fileName": name})

		case endpoint == "b2_list_file_names":
			prefix, _ := body["prefix"].(string)
			var files []map[string]any
			for name, id := range f.objects {
				if strings.HasPrefix(name, prefix) {
					files = append(files, map[string]any{"fileId": id, "fileName": name, "contentLength": 1})
				}
			}
			// Real B2 lists lexicographically, and trimBackups relies on it.
			sortByName(files)
			json.NewEncoder(w).Encode(map[string]any{"files": files, "nextFileName": ""})

		case endpoint == "b2_get_upload_url":
			json.NewEncoder(w).Encode(map[string]any{
				"uploadUrl": f.srv.URL + "/upload", "authorizationToken": "u",
			})

		case endpoint == "upload":
			name := r.Header.Get("X-Bz-File-Name")
			decoded, _ := decodeName(name)
			id := "id-" + decoded
			f.objects[decoded] = id
			json.NewEncoder(w).Encode(map[string]any{"fileId": id, "fileName": decoded})

		case endpoint == "b2_get_download_authorization":
			json.NewEncoder(w).Encode(map[string]any{"authorizationToken": "dl"})

		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func decodeName(escaped string) (string, error) {
	return strings.ReplaceAll(escaped, "%2F", "/"), nil
}

func sortByName(files []map[string]any) {
	for i := 1; i < len(files); i++ {
		for j := i; j > 0; j-- {
			a, _ := files[j-1]["fileName"].(string)
			b, _ := files[j]["fileName"].(string)
			if a <= b {
				break
			}
			files[j-1], files[j] = files[j], files[j-1]
		}
	}
}

func (f *fakeBucket) put(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[name] = "id-" + name
}

func (f *fakeBucket) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[name]
	return ok
}

func (f *fakeBucket) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for name := range f.objects {
		if strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- fixture

func newRunner(t *testing.T) (*Runner, *store.Store, *fakeBucket, context.Context) {
	t.Helper()

	bucket := newFakeBucket(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		B2KeyID: "master", B2AppKey: "s",
		B2BucketID: "bucket-1", B2BucketName: "faidz-cloud",
		B2APIBase: bucket.srv.URL,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := New(st, b2.NewService(cfg, st, log, false), log, t.TempDir())
	return runner, st, bucket, context.Background()
}

// storedFile creates a ready file and the three objects that go with it.
func storedFile(t *testing.T, st *store.Store, bucket *fakeBucket, userID, size int64, name string) *store.File {
	t.Helper()
	ctx := context.Background()

	f, err := st.BeginUpload(ctx, store.NewFile{
		UserID: userID, Filename: name, MimeType: "image/jpeg", Kind: "image",
		Size: size, Sha1: "sha-" + name, TakenAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := st.CompleteUpload(ctx, userID, f.ID, store.CompleteFile{
		B2FileID: "id-" + b2.OrigName(userID, f.ObjectKey, f.Ext), ThumbOK: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	bucket.put(b2.OrigName(userID, done.ObjectKey, done.Ext))
	bucket.put(b2.ThumbName(userID, done.ObjectKey))
	bucket.put(b2.MetaName(userID, done.ObjectKey))
	return done
}

func newUser(t *testing.T, st *store.Store) int64 {
	t.Helper()
	u, err := st.CreateUser(context.Background(), "faidz", "a-long-enough-password", 10<<30)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// ---------------------------------------------------------------- purge

func TestPurgeRemovesEveryObjectAndTheRow(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)
	file := storedFile(t, st, bucket, userID, 5<<20, "beach.jpg")

	if _, err := st.SoftDelete(ctx, userID, []int64{file.ID}); err != nil {
		t.Fatal(err)
	}
	// Backdate the deletion past the retention window.
	if _, err := st.W.ExecContext(ctx,
		`UPDATE files SET deleted_at = unixepoch() - ? WHERE id = ?`,
		int64(TrashRetention.Seconds())+3600, file.ID); err != nil {
		t.Fatal(err)
	}

	if err := runner.purge(ctx); err != nil {
		t.Fatal(err)
	}

	// All three objects must go. Leaving any of them means paying B2 for a file
	// nothing points at any more — invisible, and forever.
	for _, name := range []string{
		b2.OrigName(userID, file.ObjectKey, file.Ext),
		b2.ThumbName(userID, file.ObjectKey),
		b2.MetaName(userID, file.ObjectKey),
	} {
		if bucket.has(name) {
			t.Errorf("%s survived the purge and is still being billed", name)
		}
	}

	if _, err := st.FileByID(ctx, userID, file.ID); err == nil {
		t.Error("the row survived the purge")
	}
}

func TestPurgeSparesFilesInsideTheRetentionWindow(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)
	file := storedFile(t, st, bucket, userID, 1<<20, "recent.jpg")

	if _, err := st.SoftDelete(ctx, userID, []int64{file.ID}); err != nil {
		t.Fatal(err)
	}
	if err := runner.purge(ctx); err != nil {
		t.Fatal(err)
	}

	// Deleted a moment ago: it must still be recoverable.
	if !bucket.has(b2.OrigName(userID, file.ObjectKey, file.Ext)) {
		t.Error("a file deleted today was purged; the 30-day window is not being honoured")
	}
	if _, err := st.FileByID(ctx, userID, file.ID); err != nil {
		t.Error("the row was removed while still inside the retention window")
	}
}

func TestPurgeLeavesLiveFilesAlone(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)
	file := storedFile(t, st, bucket, userID, 1<<20, "live.jpg")

	if err := runner.purge(ctx); err != nil {
		t.Fatal(err)
	}
	if !bucket.has(b2.OrigName(userID, file.ObjectKey, file.Ext)) {
		t.Fatal("the purge deleted a file that was never in the trash")
	}
}

func TestPurgeIsSafeToRunTwice(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)
	file := storedFile(t, st, bucket, userID, 1<<20, "a.jpg")

	if _, err := st.SoftDelete(ctx, userID, []int64{file.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.W.ExecContext(ctx,
		`UPDATE files SET deleted_at = unixepoch() - ? WHERE id = ?`,
		int64(TrashRetention.Seconds())+3600, file.ID); err != nil {
		t.Fatal(err)
	}

	// A crash between deleting the object and deleting the row leaves exactly
	// the state the second pass has to survive.
	if err := runner.purge(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.purge(ctx); err != nil {
		t.Fatalf("a second purge pass failed: %v", err)
	}
}

// ---------------------------------------------------------------- orphans

func TestOrphanSweepRemovesStaleReservations(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)

	reserved, err := st.BeginUpload(ctx, store.NewFile{
		UserID: userID, Filename: "abandoned.mp4", MimeType: "video/mp4",
		Kind: "video", Size: 400 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	bucket.put(b2.OrigName(userID, reserved.ObjectKey, reserved.Ext))

	// Fresh reservations are left alone: an upload in progress is not an orphan.
	if err := runner.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FileByID(ctx, userID, reserved.ID); err != nil {
		t.Fatal("an upload in progress was swept")
	}

	if _, err := st.W.ExecContext(ctx,
		`UPDATE files SET uploaded_at = unixepoch() - ? WHERE id = ?`,
		int64(OrphanAge.Seconds())+3600, reserved.ID); err != nil {
		t.Fatal(err)
	}
	if err := runner.sweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FileByID(ctx, userID, reserved.ID); err == nil {
		t.Error("a day-old unfinished upload was not swept; it counts against the quota until it is")
	}
}

// ---------------------------------------------------------------- backup

func TestBackupUploadsAReadableDatabase(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)
	storedFile(t, st, bucket, userID, 1<<20, "a.jpg")

	if err := runner.backup(ctx); err != nil {
		t.Fatal(err)
	}

	day := time.Now().UTC().Format("20060102")
	name := b2.BackupName(userID, day)
	if !bucket.has(name) {
		t.Fatalf("no backup at %s", name)
	}

	// Running twice on the same day must not fail: VACUUM INTO refuses to
	// overwrite, so a leftover temp file from a failed run has to be cleared.
	if err := runner.backup(ctx); err != nil {
		t.Errorf("a second backup on the same day failed: %v", err)
	}
}

func TestBackupsAreTrimmedToTheRetentionCount(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)

	// Twenty days of history, oldest first.
	for day := 1; day <= 20; day++ {
		bucket.put(b2.BackupName(userID, "202601"+pad(day)))
	}
	if err := runner.backup(ctx); err != nil {
		t.Fatal(err)
	}

	prefix := strings.TrimSuffix(b2.BackupName(userID, ""), "cloud-.sqlite")
	if n := bucket.count(prefix); n != BackupRetention {
		t.Errorf("kept %d backups, want %d", n, BackupRetention)
	}
	// The oldest go first, not the newest.
	if bucket.has(b2.BackupName(userID, "20260101")) {
		t.Error("the oldest backup was kept while newer ones were deleted")
	}
}

func pad(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// ---------------------------------------------------------------- reconcile

func TestReconcileReportsDriftWithoutCorrectingIt(t *testing.T) {
	runner, st, bucket, ctx := newRunner(t)
	userID := newUser(t, st)
	storedFile(t, st, bucket, userID, 5<<20, "a.jpg")

	// Introduce drift the way a bad transaction boundary would.
	if _, err := st.W.ExecContext(ctx,
		`UPDATE users SET storage_used = storage_used + 999 WHERE id = ?`, userID); err != nil {
		t.Fatal(err)
	}

	if err := runner.reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// Silently fixing it would hide the bug that caused it, so the counter is
	// left exactly as it was.
	cached, actual, err := st.ReconcileStorageUsed(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if cached == actual {
		t.Error("reconcile corrected the drift instead of reporting it")
	}
}

// ---------------------------------------------------------------- scheduling

func TestJobsDoNotRunMoreOftenThanTheirInterval(t *testing.T) {
	runner, st, _, ctx := newRunner(t)
	newUser(t, st)

	var runs int
	count := func(context.Context) error {
		runs++
		return nil
	}

	runner.every(ctx, "probe", time.Hour, count)
	runner.every(ctx, "probe", time.Hour, count)
	runner.every(ctx, "probe", time.Hour, count)
	if runs != 1 {
		t.Errorf("job ran %d times inside its interval, want 1", runs)
	}

	// A due job runs again.
	runner.last["probe"] = time.Now().Add(-2 * time.Hour)
	runner.every(ctx, "probe", time.Hour, count)
	if runs != 2 {
		t.Errorf("job did not run when due: %d", runs)
	}
}

func TestAFailingJobDoesNotStopTheOthers(t *testing.T) {
	runner, st, _, ctx := newRunner(t)
	newUser(t, st)

	runner.every(ctx, "boom", time.Hour, func(context.Context) error {
		return context.DeadlineExceeded
	})

	// The next tick still schedules everything else.
	var ran bool
	runner.every(ctx, "after", time.Hour, func(context.Context) error {
		ran = true
		return nil
	})
	if !ran {
		t.Error("a failing job blocked the ones after it")
	}
}

func TestJobsDegradeWhenStorageIsUnconfigured(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := New(st, b2.NewService(&config.Config{}, st, log, false), log, t.TempDir())
	ctx := context.Background()

	// A server brought up before its credentials exist must still start.
	for name, fn := range map[string]func(context.Context) error{
		"purge": runner.purge, "orphans": runner.sweepOrphans,
		"tokens": runner.refreshTokens, "backup": runner.backup,
	} {
		if err := fn(ctx); err != nil {
			t.Errorf("%s failed with storage unconfigured: %v", name, err)
		}
	}
}
