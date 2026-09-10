package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// The quota accounting and the keyset pagination are where a subtle bug costs
// real money or silently loses photos, so they get the tests.

func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, context.Background()
}

func newTestUser(t *testing.T, s *Store, ctx context.Context, name string, quotaGB int64) *User {
	t.Helper()
	u, err := s.CreateUser(ctx, name, "a-long-enough-password", quotaGB<<30)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// upload runs a full init/complete cycle with a plausible payload.
func upload(t *testing.T, s *Store, ctx context.Context, userID, size int64, name string, takenAt int64) *File {
	t.Helper()
	f, err := s.BeginUpload(ctx, NewFile{
		UserID: userID, Filename: name, MimeType: "image/jpeg", Kind: "image",
		Size: size, Sha1: "sha-" + name, TakenAt: takenAt,
	})
	if err != nil {
		t.Fatalf("begin upload %s: %v", name, err)
	}
	done, err := s.CompleteUpload(ctx, userID, f.ID, CompleteFile{
		B2FileID: "b2-" + name, Sha1: "sha-" + name, ThumbOK: true, ThumbTier: "native",
		ThumbFormat: "image/webp", TakenAt: takenAt,
	})
	if err != nil {
		t.Fatalf("complete upload %s: %v", name, err)
	}
	return done
}

func storageUsed(t *testing.T, s *Store, ctx context.Context, userID int64) int64 {
	t.Helper()
	u, err := s.UserByID(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.StorageUsed
}

// ---------------------------------------------------------------- quota

func TestQuotaChargedOnlyOnCompletion(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 1)

	f, err := s.BeginUpload(ctx, NewFile{
		UserID: u.ID, Filename: "a.jpg", MimeType: "image/jpeg", Kind: "image", Size: 5 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bytes that are only reserved are not yet stored, so they must not be
	// charged: an upload that fails halfway would otherwise leak quota.
	if got := storageUsed(t, s, ctx, u.ID); got != 0 {
		t.Errorf("storage_used is %d before completion, want 0", got)
	}

	if _, err := s.CompleteUpload(ctx, u.ID, f.ID, CompleteFile{B2FileID: "b2-1", ThumbOK: true}); err != nil {
		t.Fatal(err)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 5<<20 {
		t.Errorf("storage_used is %d after completion, want %d", got, 5<<20)
	}
}

func TestQuotaCountsUploadsInFlight(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 1) // 1 GB

	// Reserve most of the quota without completing anything.
	for i := 0; i < 3; i++ {
		if _, err := s.BeginUpload(ctx, NewFile{
			UserID: u.ID, Filename: "big.mp4", MimeType: "video/mp4", Kind: "video", Size: 300 << 20,
		}); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}

	// A fourth would fit if only completed bytes counted — storage_used is
	// still zero — but 4 x 300 MB does not fit in 1 GB. Counting in-flight
	// uploads is what stops a burst of parallel inits from overshooting.
	_, err := s.BeginUpload(ctx, NewFile{
		UserID: u.ID, Filename: "one-too-many.mp4", MimeType: "video/mp4", Kind: "video", Size: 300 << 20,
	})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
}

func TestAbandonedUploadReleasesReservation(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 1)

	reserve := func(name string) (*File, error) {
		return s.BeginUpload(ctx, NewFile{
			UserID: u.ID, Filename: name, MimeType: "video/mp4", Kind: "video", Size: 400 << 20,
		})
	}

	first, err := reserve("a.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reserve("b.mp4"); err != nil {
		t.Fatal(err)
	}
	// 3 x 400 MB does not fit in 1 GB.
	if _, err := reserve("c.mp4"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}

	if err := s.AbandonUpload(ctx, u.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	// With the abandoned reservation gone, the space is usable again.
	if _, err := reserve("c.mp4"); err != nil {
		t.Errorf("space was not released: %v", err)
	}
}

func TestDeleteRefundsQuotaExactlyOnce(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 1)

	f := upload(t, s, ctx, u.ID, 10<<20, "a.jpg", 1000)
	if got := storageUsed(t, s, ctx, u.ID); got != 10<<20 {
		t.Fatalf("storage_used = %d", got)
	}

	if _, err := s.SoftDelete(ctx, u.ID, []int64{f.ID}); err != nil {
		t.Fatal(err)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 0 {
		t.Errorf("after delete storage_used = %d, want 0", got)
	}

	// Deleting an already-deleted file must not subtract the bytes a second
	// time, which would drive the counter negative and hand out free quota.
	if _, err := s.SoftDelete(ctx, u.ID, []int64{f.ID}); err != nil {
		t.Fatal(err)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 0 {
		t.Errorf("double delete changed storage_used to %d, want 0", got)
	}

	if _, err := s.Restore(ctx, u.ID, []int64{f.ID}); err != nil {
		t.Fatal(err)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 10<<20 {
		t.Errorf("after restore storage_used = %d, want %d", got, 10<<20)
	}
}

func TestStorageUsedMatchesRowsAfterChurn(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	var ids []int64
	for i := 0; i < 12; i++ {
		f := upload(t, s, ctx, u.ID, int64(i+1)<<20, "f.jpg", int64(1000+i))
		ids = append(ids, f.ID)
	}
	if _, err := s.SoftDelete(ctx, u.ID, ids[:5]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, u.ID, ids[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SoftDelete(ctx, u.ID, ids[8:]); err != nil {
		t.Fatal(err)
	}

	// The counter is a cache of a sum. Drift means a transaction boundary is
	// wrong somewhere, which is exactly what the weekly reconcile job watches
	// for (spec §7.2).
	cached, actual, err := s.ReconcileStorageUsed(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cached != actual {
		t.Errorf("storage_used drifted: cached %d, actual %d", cached, actual)
	}
}

// ---------------------------------------------------------------- pagination

func TestKeysetPaginationCoversEveryRowOnce(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	// Deliberate ties on taken_at: a burst import gives many photos the same
	// timestamp, and a cursor keyed on it alone would skip or repeat rows.
	const total = 37
	want := map[int64]bool{}
	for i := 0; i < total; i++ {
		f := upload(t, s, ctx, u.ID, 1<<20, "f.jpg", int64(1000+i/5))
		want[f.ID] = true
	}

	seen := map[int64]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
		res, err := s.ListFiles(ctx, u.ID, ListFilter{Limit: 7, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range res.Files {
			seen[f.ID]++
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	if len(seen) != total {
		t.Errorf("walked %d rows, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %d returned %d times", id, n)
		}
	}
	for id := range want {
		if seen[id] == 0 {
			t.Errorf("row %d was never returned", id)
		}
	}
}

func TestListOrdersByTakenAtNotUploadTime(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	// Uploaded last, taken first — a bulk import of old photos.
	upload(t, s, ctx, u.ID, 1<<20, "new.jpg", 2000)
	oldPhoto := upload(t, s, ctx, u.ID, 1<<20, "old.jpg", 1000)
	newest := upload(t, s, ctx, u.ID, 1<<20, "newest.jpg", 3000)

	res, err := s.ListFiles(ctx, u.ID, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 3 {
		t.Fatalf("got %d files", len(res.Files))
	}
	if res.Files[0].ID != newest.ID {
		t.Error("newest photo is not first")
	}
	if res.Files[2].ID != oldPhoto.ID {
		t.Error("oldest photo is not last; the gallery is sorted by upload time")
	}
}

func TestListExcludesIncompleteAndDeleted(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	ready := upload(t, s, ctx, u.ID, 1<<20, "ready.jpg", 1000)
	trashed := upload(t, s, ctx, u.ID, 1<<20, "trashed.jpg", 1001)
	if _, err := s.SoftDelete(ctx, u.ID, []int64{trashed.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginUpload(ctx, NewFile{
		UserID: u.ID, Filename: "inflight.jpg", MimeType: "image/jpeg", Kind: "image", Size: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := s.ListFiles(ctx, u.ID, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].ID != ready.ID {
		t.Errorf("gallery should show only the one ready file, got %d", len(res.Files))
	}

	trash, err := s.ListFiles(ctx, u.ID, ListFilter{Deleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(trash.Files) != 1 || trash.Files[0].ID != trashed.ID {
		t.Errorf("trash view is wrong: %d files", len(trash.Files))
	}
}

func TestSearchTreatsWildcardsAsLiterals(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	upload(t, s, ctx, u.ID, 1<<20, "battery 100% charged.jpg", 1000)
	upload(t, s, ctx, u.ID, 1<<20, "holiday.jpg", 1001)

	res, err := s.ListFiles(ctx, u.ID, ListFilter{Query: "100%"})
	if err != nil {
		t.Fatal(err)
	}
	// Unescaped, "100%" as a LIKE pattern would also match "holiday.jpg".
	if len(res.Files) != 1 {
		t.Errorf("searching for a literal %% matched %d files, want 1", len(res.Files))
	}
}

func TestRejectsMalformedCursor(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	if _, err := s.ListFiles(ctx, u.ID, ListFilter{Cursor: "not-a-cursor!!"}); !errors.Is(err, ErrBadCursor) {
		t.Errorf("want ErrBadCursor, got %v", err)
	}
}

// ---------------------------------------------------------------- isolation

func TestFilesAreIsolatedBetweenUsers(t *testing.T) {
	s, ctx := newTestStore(t)
	a := newTestUser(t, s, ctx, "alice", 10)
	b := newTestUser(t, s, ctx, "bob", 10)

	f := upload(t, s, ctx, a.ID, 1<<20, "private.jpg", 1000)

	// Every query filters on user_id. Bob asking for Alice's id must get
	// "not found", never the row.
	if _, err := s.FileByID(ctx, b.ID, f.ID); !errors.Is(err, ErrNoFile) {
		t.Errorf("cross-user read returned %v, want ErrNoFile", err)
	}
	if err := s.RenameFile(ctx, b.ID, f.ID, "stolen.jpg"); !errors.Is(err, ErrNoFile) {
		t.Errorf("cross-user rename returned %v, want ErrNoFile", err)
	}
	if n, _ := s.SoftDelete(ctx, b.ID, []int64{f.ID}); n != 0 {
		t.Error("cross-user delete affected rows")
	}
	if got := storageUsed(t, s, ctx, a.ID); got != 1<<20 {
		t.Errorf("another user's delete changed alice's quota to %d", got)
	}
}

// ---------------------------------------------------------------- dedup

func TestFindDuplicateIgnoresTrashAndOtherUsers(t *testing.T) {
	s, ctx := newTestStore(t)
	a := newTestUser(t, s, ctx, "alice", 10)
	b := newTestUser(t, s, ctx, "bob", 10)

	f := upload(t, s, ctx, a.ID, 1<<20, "shared.jpg", 1000)

	if dup, err := s.FindDuplicate(ctx, a.ID, "sha-shared.jpg"); err != nil || dup == nil {
		t.Fatalf("own duplicate not found: %v %v", dup, err)
	}
	if dup, _ := s.FindDuplicate(ctx, b.ID, "sha-shared.jpg"); dup != nil {
		t.Error("deduplication leaked across users")
	}

	if _, err := s.SoftDelete(ctx, a.ID, []int64{f.ID}); err != nil {
		t.Fatal(err)
	}
	// A trashed file should not block re-uploading the same photo.
	if dup, _ := s.FindDuplicate(ctx, a.ID, "sha-shared.jpg"); dup != nil {
		t.Error("a trashed file was reported as a duplicate")
	}
}

// ---------------------------------------------------------------- folders

func TestDeleteFolderRefusesWhenItStillHasFiles(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	folder, err := s.CreateFolder(ctx, u.ID, "Holiday")
	if err != nil {
		t.Fatal(err)
	}
	f := upload(t, s, ctx, u.ID, 1<<20, "beach.jpg", 1000)
	if err := s.MoveFile(ctx, u.ID, f.ID, &folder.ID); err != nil {
		t.Fatal(err)
	}

	// Deleting a folder full of photos is not obviously the same request as
	// deleting the folder.
	if _, err := s.DeleteFolder(ctx, u.ID, folder.ID, DeleteRefuse); !errors.Is(err, ErrFolderNotEmpty) {
		t.Errorf("want ErrFolderNotEmpty, got %v", err)
	}

	moved, err := s.DeleteFolder(ctx, u.ID, folder.ID, DeleteMoveToRoot)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Errorf("moved %d files to root, want 1", moved)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 1<<20 {
		t.Errorf("moving files to root changed the quota to %d", got)
	}

	reloaded, err := s.FileByID(ctx, u.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.FolderID != nil {
		t.Error("file was not moved to the root")
	}
}

func TestCascadeDeleteTrashesRatherThanDestroys(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	folder, _ := s.CreateFolder(ctx, u.ID, "Old")
	f := upload(t, s, ctx, u.ID, 3<<20, "x.jpg", 1000)
	if err := s.MoveFile(ctx, u.ID, f.ID, &folder.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeleteFolder(ctx, u.ID, folder.ID, DeleteCascade); err != nil {
		t.Fatal(err)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 0 {
		t.Errorf("cascade did not release the quota: %d", got)
	}

	// Soft, not hard: the files stay recoverable for thirty days.
	trash, err := s.ListFiles(ctx, u.ID, ListFilter{Deleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(trash.Files) != 1 {
		t.Fatalf("cascade destroyed the files instead of trashing them")
	}
	if _, err := s.Restore(ctx, u.ID, []int64{f.ID}); err != nil {
		t.Fatal(err)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 3<<20 {
		t.Errorf("restore after cascade left storage_used at %d", got)
	}
}

func TestDuplicateFolderNameRejected(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	if _, err := s.CreateFolder(ctx, u.ID, "Trips"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(ctx, u.ID, "Trips"); !errors.Is(err, ErrFolderExists) {
		t.Errorf("want ErrFolderExists, got %v", err)
	}
	// A different user may of course use the same name.
	other := newTestUser(t, s, ctx, "bob", 10)
	if _, err := s.CreateFolder(ctx, other.ID, "Trips"); err != nil {
		t.Errorf("folder names should be per-user: %v", err)
	}
}

func TestMoveToUnknownFolderRejected(t *testing.T) {
	s, ctx := newTestStore(t)
	a := newTestUser(t, s, ctx, "alice", 10)
	b := newTestUser(t, s, ctx, "bob", 10)

	bobFolder, _ := s.CreateFolder(ctx, b.ID, "Bob's")
	f := upload(t, s, ctx, a.ID, 1<<20, "a.jpg", 1000)

	// Filing your photo into someone else's folder must not work.
	if err := s.MoveFile(ctx, a.ID, f.ID, &bobFolder.ID); !errors.Is(err, ErrNoFolder) {
		t.Errorf("want ErrNoFolder, got %v", err)
	}
}

// ---------------------------------------------------------------- limits

func TestOversizedUploadRejected(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 100)

	if _, err := s.BeginUpload(ctx, NewFile{
		UserID: u.ID, Filename: "huge.mov", MimeType: "video/quicktime", Kind: "video",
		Size: MaxFileSize + 1,
	}); err == nil {
		t.Error("a file over the 500 MB ceiling was accepted")
	}
}

func TestCompleteIsNotIdempotent(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	f := upload(t, s, ctx, u.ID, 4<<20, "a.jpg", 1000)

	// Completing twice would charge the quota twice.
	if _, err := s.CompleteUpload(ctx, u.ID, f.ID, CompleteFile{B2FileID: "b2-again", ThumbOK: true}); !errors.Is(err, ErrWrongState) {
		t.Errorf("want ErrWrongState on a second completion, got %v", err)
	}
	if got := storageUsed(t, s, ctx, u.ID); got != 4<<20 {
		t.Errorf("storage_used = %d after a repeated completion, want %d", got, 4<<20)
	}
}

func TestUnsupportedThumbnailQueuesForFallback(t *testing.T) {
	s, ctx := newTestStore(t)
	u := newTestUser(t, s, ctx, "faidz", 10)

	f, err := s.BeginUpload(ctx, NewFile{
		UserID: u.ID, Filename: "clip.mov", MimeType: "video/quicktime", Kind: "video", Size: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Chrome on desktop cannot decode HEVC, so the client reports no thumbnail.
	done, err := s.CompleteUpload(ctx, u.ID, f.ID, CompleteFile{B2FileID: "b2-1", ThumbOK: false})
	if err != nil {
		t.Fatal(err)
	}
	if done.ThumbStatus != "unsupported" {
		t.Errorf("thumb_status = %q, want unsupported so the tier 3 worker picks it up", done.ThumbStatus)
	}
	if done.HasThumb {
		t.Error("has_thumb is set despite no thumbnail being uploaded")
	}

	stats, err := s.StorageStats(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 {
		t.Errorf("thumbnails_pending = %d, want 1", stats.Pending)
	}
}
