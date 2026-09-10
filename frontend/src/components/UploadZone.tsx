import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";

import { uploadQueue } from "../upload/queue";

/** What the picker offers. iOS needs the extensions spelled out, not just image/*. */
const ACCEPT = "image/*,video/*,.heic,.HEIC,.heif,.HEIF,.dng,.DNG,.mov,.MOV";

/**
 * Wraps the gallery so a file dropped anywhere in the window is picked up, and
 * exposes a button for the ordinary case.
 */
export function UploadZone({
  folderId,
  children,
}: {
  folderId?: number;
  children: ReactNode;
}) {
  const [dragging, setDragging] = useState(false);
  const depth = useRef(0);

  const accept = useCallback(
    (files: FileList | null) => {
      if (!files || files.length === 0) return;
      uploadQueue.add(Array.from(files), folderId);
    },
    [folderId],
  );

  useEffect(() => {
    // dragenter and dragleave fire for every child element the cursor crosses,
    // so a counter is what keeps the highlight from flickering.
    const onEnter = (event: DragEvent) => {
      if (!event.dataTransfer?.types.includes("Files")) return;
      event.preventDefault();
      depth.current += 1;
      setDragging(true);
    };
    const onOver = (event: DragEvent) => event.preventDefault();
    const onLeave = () => {
      depth.current = Math.max(0, depth.current - 1);
      if (depth.current === 0) setDragging(false);
    };
    const onDrop = (event: DragEvent) => {
      event.preventDefault();
      depth.current = 0;
      setDragging(false);
      accept(event.dataTransfer?.files ?? null);
    };

    window.addEventListener("dragenter", onEnter);
    window.addEventListener("dragover", onOver);
    window.addEventListener("dragleave", onLeave);
    window.addEventListener("drop", onDrop);
    return () => {
      window.removeEventListener("dragenter", onEnter);
      window.removeEventListener("dragover", onOver);
      window.removeEventListener("dragleave", onLeave);
      window.removeEventListener("drop", onDrop);
    };
  }, [accept]);

  return (
    <div className="relative flex min-h-0 flex-1 flex-col">
      {children}

      {dragging && (
        <div className="pointer-events-none absolute inset-2 z-10 flex items-center justify-center rounded-sm border-2 border-dashed border-accent bg-accent-soft/80">
          <p className="text-sm font-semibold text-ink">Drop to upload</p>
        </div>
      )}
    </div>
  );
}

export function UploadButton({ folderId }: { folderId?: number }) {
  const input = useRef<HTMLInputElement>(null);

  return (
    <>
      <button
        type="button"
        onClick={() => input.current?.click()}
        className="rounded-sm bg-accent px-3 py-1.5 text-sm font-semibold text-accent-ink transition hover:brightness-110"
      >
        Upload
      </button>
      <input
        ref={input}
        type="file"
        multiple
        accept={ACCEPT}
        hidden
        onChange={(event) => {
          if (event.target.files) uploadQueue.add(Array.from(event.target.files), folderId);
          // Reset so picking the same file twice in a row still fires a change.
          event.target.value = "";
        }}
      />
    </>
  );
}
