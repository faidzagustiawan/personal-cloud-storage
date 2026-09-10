import { useEffect, useRef, useState } from "react";

import { canPlay, useDeleteFiles, useRenameFile, useRestoreFile } from "../hooks/useFiles";
import { formatBytes, formatDateTime, formatDuration } from "../lib/format";
import type { FileItem } from "../lib/types";
import { ShareDialog } from "./ShareDialog";
import { Button, Notice } from "./ui";

interface ViewerProps {
  files: FileItem[];
  index: number;
  onIndexChange: (index: number) => void;
  onClose: () => void;
  trash?: boolean;
}

/**
 * A modal, not a route.
 *
 * Closing it returns to the grid exactly as it was — same scroll offset, same
 * loaded pages — where a route change would remount the gallery and throw all
 * of that away.
 *
 * Built on a native <dialog> so the focus trap, the inert background and
 * Escape-to-close are the browser's rather than a hand-rolled imitation.
 */
export function Viewer({ files, index, onIndexChange, onClose, trash = false }: ViewerProps) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const file = files[index];

  // Kept in a ref so the mount effect below can stay mount-only.
  const closeRef = useRef(onClose);
  closeRef.current = onClose;

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (!dialog.open) dialog.showModal();

    // React owns whether the viewer exists; the dialog only ever closes because
    // this component unmounted and the cleanup below closed it.
    //
    // Escape is intercepted rather than left to the browser. Left alone it
    // dismisses the dialog directly, and the `close` event that is supposed to
    // tell us has proven unreliable — when it does not arrive, React still
    // believes the viewer is open, the dialog is off-screen, and it never
    // reopens because the showModal above only runs on mount. Both events are
    // still listened for, in case the browser does deliver them.
    const requestClose = (event?: Event) => {
      event?.preventDefault();
      closeRef.current();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") requestClose(event);
    };

    dialog.addEventListener("keydown", onKeyDown);
    dialog.addEventListener("cancel", requestClose);
    dialog.addEventListener("close", () => closeRef.current());

    return () => {
      dialog.removeEventListener("keydown", onKeyDown);
      dialog.removeEventListener("cancel", requestClose);
      if (dialog.open) dialog.close();
    };
  }, []);

  // Held-down arrow keys autorepeat faster than React re-renders, so the
  // handler reads the position from a ref rather than from the closed-over
  // prop. Bound once: several presses inside one tick would otherwise all see
  // the same stale index and cancel each other out.
  const indexRef = useRef(index);
  indexRef.current = index;
  const countRef = useRef(files.length);
  countRef.current = files.length;

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      // The ref advances here rather than waiting for the re-render, so a burst
      // of presses steps once per press instead of all landing on the same
      // starting position and undoing one another.
      if (event.key === "ArrowRight" && indexRef.current < countRef.current - 1) {
        indexRef.current += 1;
        onIndexChange(indexRef.current);
      }
      if (event.key === "ArrowLeft" && indexRef.current > 0) {
        indexRef.current -= 1;
        onIndexChange(indexRef.current);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onIndexChange]);

  if (!file) return null;

  return (
    <dialog
      ref={dialogRef}
      // Clicking the backdrop is the dialog element itself; clicking anything
      // inside is a descendant, so this closes on backdrop clicks only.
      onClick={(event) => {
        if (event.target === dialogRef.current) onClose();
      }}
      className="m-0 h-full max-h-none w-full max-w-none bg-ground p-0 text-ink backdrop:bg-black/70"
    >
      <div className="flex h-full flex-col">
        <ViewerHeader
          file={file}
          position={`${index + 1} / ${files.length}`}
          onClose={onClose}
        />

        <div className="relative flex min-h-0 flex-1 items-center justify-center bg-black/85">
          <Stage file={file} />

          <NavButton
            side="left"
            disabled={index === 0}
            onClick={() => onIndexChange(index - 1)}
          />
          <NavButton
            side="right"
            disabled={index >= files.length - 1}
            onClick={() => onIndexChange(index + 1)}
          />
        </div>

        <ViewerFooter file={file} trash={trash} onClose={onClose} />
      </div>
    </dialog>
  );
}

// ---------------------------------------------------------------- stage

function Stage({ file }: { file: FileItem }) {
  const [playable] = useState(() => canPlay(file));

  if (file.kind === "video") {
    if (!playable) {
      // Solving the thumbnail did not solve playback: an HEVC clip has a poster
      // frame from the server-side worker but Chrome still cannot decode it.
      // Saying so beats an empty player with no explanation (spec §5.4).
      return (
        <div className="flex max-w-md flex-col items-center gap-4 p-6 text-center">
          {file.thumb_url && (
            <img src={file.thumb_url} alt="" className="max-h-64 rounded-sm object-contain" />
          )}
          <div className="flex flex-col gap-1">
            <p className="text-sm font-semibold text-white">This browser cannot play this clip</p>
            <p className="text-sm text-white/70">
              It is stored intact and plays on the device that recorded it. Download it to watch
              here.
            </p>
          </div>
          <a
            href={file.orig_url}
            target="_blank"
            rel="noopener noreferrer"
            className="rounded-sm bg-accent px-4 py-2 text-sm font-semibold text-accent-ink"
          >
            Download
          </a>
        </div>
      );
    }

    return (
      <video
        key={file.id}
        src={file.orig_url}
        poster={file.thumb_url}
        controls
        playsInline
        preload="metadata"
        className="max-h-full max-w-full"
      />
    );
  }

  return (
    <img
      key={file.id}
      src={file.orig_url}
      alt={file.filename}
      className="max-h-full max-w-full object-contain"
    />
  );
}

function NavButton({
  side,
  disabled,
  onClick,
}: {
  side: "left" | "right";
  disabled: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      aria-label={side === "left" ? "Previous" : "Next"}
      className={`absolute ${side === "left" ? "left-2" : "right-2"} top-1/2 flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white transition hover:bg-black/70 disabled:pointer-events-none disabled:opacity-0`}
    >
      <svg width="18" height="18" viewBox="0 0 18 18" aria-hidden="true">
        <path
          d={side === "left" ? "M11.5 3.5 6 9l5.5 5.5" : "M6.5 3.5 12 9l-5.5 5.5"}
          fill="none"
          stroke="currentColor"
          strokeWidth="1.8"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </button>
  );
}

// ---------------------------------------------------------------- chrome

function ViewerHeader({
  file,
  position,
  onClose,
}: {
  file: FileItem;
  position: string;
  onClose: () => void;
}) {
  const rename = useRenameFile();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(file.filename);

  useEffect(() => {
    setName(file.filename);
    setEditing(false);
  }, [file.id, file.filename]);

  return (
    <header className="flex items-center gap-3 border-b border-line px-3 py-2">
      {editing ? (
        <form
          className="flex min-w-0 flex-1 items-center gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            rename.mutate(
              { id: file.id, filename: name },
              { onSuccess: () => setEditing(false) },
            );
          }}
        >
          <input
            autoFocus
            value={name}
            onChange={(event) => setName(event.target.value)}
            onBlur={() => setEditing(false)}
            className="min-w-0 flex-1 rounded-sm border border-line bg-surface px-2 py-1 text-sm"
          />
          <Button type="submit" loading={rename.isPending}>
            Save
          </Button>
        </form>
      ) : (
        <button
          type="button"
          onClick={() => setEditing(true)}
          title="Rename"
          className="min-w-0 flex-1 truncate text-left text-sm font-semibold text-ink hover:text-accent"
        >
          {file.filename}
        </button>
      )}

      <span className="tabular shrink-0 text-xs text-ink-3">{position}</span>
      <button
        type="button"
        onClick={onClose}
        aria-label="Close"
        className="shrink-0 rounded-sm border border-line px-2 py-1 text-ink-2 hover:text-ink"
      >
        <svg width="14" height="14" viewBox="0 0 14 14" aria-hidden="true">
          <path d="M3 3l8 8M11 3l-8 8" stroke="currentColor" strokeWidth="1.6" fill="none" />
        </svg>
      </button>
    </header>
  );
}

function ViewerFooter({
  file,
  trash,
  onClose,
}: {
  file: FileItem;
  trash: boolean;
  onClose: () => void;
}) {
  const remove = useDeleteFiles();
  const restore = useRestoreFile();
  const [sharing, setSharing] = useState(false);

  return (
    <footer className="flex flex-wrap items-center gap-x-6 gap-y-2 border-t border-line px-3 py-2">
      <dl className="flex flex-wrap items-baseline gap-x-5 gap-y-1 text-xs">
        <Fact label="Taken">{formatDateTime(file.taken_at)}</Fact>
        <Fact label="Size">{formatBytes(file.file_size, true)}</Fact>
        {file.width && file.height && (
          <Fact label="Dimensions">
            {file.width} × {file.height}
          </Fact>
        )}
        {file.duration_sec ? (
          <Fact label="Length">{formatDuration(file.duration_sec)}</Fact>
        ) : null}
        <Fact label="Type">{file.mime_type}</Fact>
      </dl>

      <div className="flex flex-1 justify-end gap-2">
        <a
          href={file.orig_url}
          target="_blank"
          rel="noopener noreferrer"
          className="rounded-sm border border-line px-3 py-1.5 text-sm font-medium text-ink-2 hover:bg-surface-2 hover:text-ink"
        >
          Download
        </a>

        {/* A file on its way to being purged cannot be shared, so the trash
            offers Restore in that slot instead. */}
        {!trash && (
          <Button variant="ghost" onClick={() => setSharing(true)}>
            Share
          </Button>
        )}

        {trash ? (
          <Button
            variant="ghost"
            loading={restore.isPending}
            onClick={() => restore.mutate(file.id, { onSuccess: onClose })}
          >
            Restore
          </Button>
        ) : (
          <Button
            variant="danger"
            loading={remove.isPending}
            onClick={() => remove.mutate([file.id], { onSuccess: onClose })}
          >
            Delete
          </Button>
        )}
      </div>

      {sharing && <ShareDialog file={file} onClose={() => setSharing(false)} />}

      {(remove.error || restore.error) && (
        <div className="w-full">
          <Notice>{(remove.error ?? restore.error)?.message}</Notice>
        </div>
      )}
    </footer>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-baseline gap-1.5">
      <dt className="text-[10px] tracking-wider text-ink-3 uppercase">{label}</dt>
      <dd className="tabular text-ink">{children}</dd>
    </div>
  );
}
