import { formatBytes, formatDate, formatDuration } from "../../lib/format";
import type { FileItem } from "../../lib/types";

export const TILE_MIN_WIDTH = 168;
export const TILE_GAP = 10;
export const TILE_CAPTION_HEIGHT = 44;

interface TileProps {
  file: FileItem;
  selected: boolean;
  selecting: boolean;
  onOpen: () => void;
  onToggle: (event: React.MouseEvent) => void;
}

export function Tile({ file, selected, selecting, onOpen, onToggle }: TileProps) {
  return (
    <div
      className={`group relative flex h-full flex-col overflow-hidden rounded-sm border bg-surface transition ${
        selected ? "border-accent ring-1 ring-accent" : "border-line"
      }`}
    >
      <button
        type="button"
        onClick={onOpen}
        className="relative block aspect-square shrink-0 bg-surface-2 text-left"
        aria-label={`Open ${file.filename}`}
      >
        {/* Absolutely positioned so the box keeps its square: an in-flow image
            sizes the container to its own ratio and aspect-square loses, which
            leaves portrait tiles taller than landscape ones. */}
        {file.thumb_url ? (
          <img
            src={file.thumb_url}
            alt=""
            loading="lazy"
            decoding="async"
            className="absolute inset-0 h-full w-full object-cover"
          />
        ) : (
          <span className="tabular absolute inset-0 flex items-center justify-center px-2 text-center text-[10px] leading-tight tracking-wider text-ink-3 uppercase">
            {file.thumb_status === "unsupported" ? "preview\npending" : "no preview"}
          </span>
        )}

        {file.kind === "video" && (
          <span className="tabular absolute right-1 bottom-1 rounded-xs bg-black/65 px-1 py-0.5 text-[10px] text-white">
            {file.duration_sec ? formatDuration(file.duration_sec) : "video"}
          </span>
        )}
      </button>

      {/* The checkbox stays out of the way until it is wanted: visible on hover,
          on keyboard focus, and whenever a selection is already running. */}
      <button
        type="button"
        onClick={onToggle}
        aria-pressed={selected}
        aria-label={selected ? `Deselect ${file.filename}` : `Select ${file.filename}`}
        className={`absolute top-1.5 left-1.5 flex h-5 w-5 items-center justify-center rounded-xs border transition ${
          selected
            ? "border-accent bg-accent text-accent-ink"
            : "border-white/70 bg-black/35 text-transparent group-hover:text-white/80"
        } ${selecting || selected ? "opacity-100" : "opacity-0 group-hover:opacity-100 focus-visible:opacity-100"}`}
      >
        <svg width="11" height="11" viewBox="0 0 12 12" aria-hidden="true">
          <path
            d="M2 6.5 4.5 9 10 3"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </button>

      <div className="flex min-w-0 flex-col justify-center gap-0.5 px-2 py-1.5">
        <p className="truncate text-xs font-medium text-ink" title={file.filename}>
          {file.filename}
        </p>
        <p className="tabular truncate text-[11px] text-ink-3">
          {formatDate(file.taken_at)} · {formatBytes(file.file_size)}
        </p>
      </div>
    </div>
  );
}
