package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"cloudapp/internal/b2"
	"cloudapp/internal/store"
)

// The tier 3 thumbnail worker of spec §5.4.
//
// It exists so that "everything displays properly" survives the formats no
// browser can decode — chiefly HEVC video opened anywhere but Safari. Expected
// hit rate is near zero for uploads from the phone; it matters for a desktop
// bulk import, where it turns a wall of grey placeholders into a real gallery.

const (
	// thumbTimeout bounds a single extraction. A clip that has not produced one
	// frame in a minute is not going to.
	thumbTimeout = 60 * time.Second
	// thumbEdge matches the client-side thumbnails so the grid stays uniform.
	thumbEdge = 400
)

type thumbnailWorker struct {
	store *store.Store
	b2    *b2.Service
	log   *slog.Logger

	// ffmpeg is resolved once. Its absence degrades the fallback rather than
	// crashing the server: the rows simply stay unsupported.
	once   sync.Once
	ffmpeg string
	nice   string
}

func newThumbnailWorker(st *store.Store, storage *b2.Service, log *slog.Logger) *thumbnailWorker {
	return &thumbnailWorker{store: st, b2: storage, log: log}
}

func (w *thumbnailWorker) resolve() {
	w.once.Do(func() {
		if path, err := exec.LookPath("ffmpeg"); err == nil {
			w.ffmpeg = path
		} else {
			w.log.Warn("ffmpeg is not installed; files the browser could not decode will keep a placeholder (spec §5.4)")
		}
		if runtime.GOOS != "windows" {
			if path, err := exec.LookPath("nice"); err == nil {
				w.nice = path
			}
		}
	})
}

func (w *thumbnailWorker) available() bool {
	w.resolve()
	return w.ffmpeg != ""
}

// runOne processes a single file. One at a time on purpose: the VPS is sized
// for JSON, and this is the only thing on it that decodes video.
func (w *thumbnailWorker) runOne(ctx context.Context) error {
	if !w.available() || !w.b2.Configured() {
		return nil
	}

	file, err := w.store.ClaimThumbnailJob(ctx, maxThumbAttempts)
	if err != nil || file == nil {
		return err
	}

	thumb, err := w.extract(ctx, file)
	if err != nil {
		w.log.Warn("thumbnail extraction failed",
			"object_key", file.ObjectKey, "attempt", file.ThumbAttempts, "err", err)

		// The attempt counter was already incremented when the row was claimed,
		// so this is the last try if it has reached the ceiling.
		if file.ThumbAttempts >= maxThumbAttempts {
			if err := w.store.MarkThumbnailFailed(ctx, file.ID); err != nil {
				return err
			}
			w.log.Info("giving up on a thumbnail", "object_key", file.ObjectKey)
		}
		return nil
	}

	if err := w.b2.PutThumbnail(ctx, file.UserID, file.ObjectKey, thumb, "image/webp"); err != nil {
		return fmt.Errorf("upload thumbnail: %w", err)
	}
	if err := w.store.MarkThumbnailReady(ctx, file.ID, "ffmpeg", "image/webp"); err != nil {
		return err
	}

	w.log.Info("thumbnail extracted on the server",
		"object_key", file.ObjectKey, "kind", file.Kind, "bytes", len(thumb))
	return nil
}

// extract pulls one frame out of the stored object.
//
// The URL is signed and handed to ffmpeg, which fetches over HTTP with byte
// ranges rather than downloading the whole file: for a .MOV the moov atom sits
// at the end, so ffmpeg reads the tail for the sample table and then seeks
// straight to the frame it wants. A 200 MB video costs a few MB of transfer.
func (w *thumbnailWorker) extract(ctx context.Context, file *store.File) ([]byte, error) {
	name := b2.OrigName(file.UserID, file.ObjectKey, file.Ext)
	url, err := w.b2.SignedURL(ctx, name, thumbTimeout+30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("sign url: %w", err)
	}

	out := filepath.Join(os.TempDir(), fmt.Sprintf("thumb-%s.webp", file.ObjectKey))
	defer os.Remove(out)

	// Seeking before -i is an input seek: ffmpeg jumps using the index instead
	// of decoding everything up to that point.
	offset := "1"
	if file.DurationSec != nil && *file.DurationSec > 0 {
		offset = strconv.FormatFloat(*file.DurationSec*0.1, 'f', 2, 64)
	}

	args := []string{
		"-nostdin", "-loglevel", "error",
		"-ss", offset,
		"-i", url,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", thumbEdge),
		"-f", "webp", "-y", out,
	}

	name0, argv := w.ffmpeg, args
	if w.nice != "" {
		// The VPS has other work; this is the least important thing on it.
		name0, argv = w.nice, append([]string{"-n", "19", w.ffmpeg}, args...)
	}

	runCtx, cancel := context.WithTimeout(ctx, thumbTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name0, argv...)
	stderr, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("ffmpeg timed out after %s", thumbTimeout)
		}
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, trim(string(stderr)))
	}

	body, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg produced no output: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("ffmpeg produced an empty file")
	}
	return body, nil
}

// trim keeps a log line to the decisive part of ffmpeg's output.
func trim(s string) string {
	const max = 200
	s = filepath.ToSlash(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
