import { useEffect, useState } from "react";

import { UploadButton } from "../UploadZone";

interface ToolbarProps {
  count: number;
  hasMore: boolean;
  query: string;
  onQueryChange: (value: string) => void;
  kind: "" | "image" | "video";
  onKindChange: (value: "" | "image" | "video") => void;
  folderId?: number;
  trash: boolean;
}

export function Toolbar({
  count,
  hasMore,
  query,
  onQueryChange,
  kind,
  onKindChange,
  folderId,
  trash,
}: ToolbarProps) {
  // Typed text is local so the field stays responsive; the query it drives is
  // debounced, or every keystroke would be a round trip.
  const [text, setText] = useState(query);

  useEffect(() => setText(query), [query]);

  useEffect(() => {
    if (text === query) return;
    const timer = window.setTimeout(() => onQueryChange(text), 250);
    return () => window.clearTimeout(timer);
  }, [text, query, onQueryChange]);

  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
      <label className="relative flex min-w-40 flex-1 items-center sm:max-w-64">
        <span className="sr-only">Search by filename</span>
        <svg
          width="14"
          height="14"
          viewBox="0 0 14 14"
          aria-hidden="true"
          className="pointer-events-none absolute left-2 text-ink-3"
        >
          <circle cx="6" cy="6" r="4.2" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M9.2 9.2 12.5 12.5" stroke="currentColor" strokeWidth="1.5" />
        </svg>
        <input
          type="search"
          value={text}
          onChange={(event) => setText(event.target.value)}
          placeholder="Search filenames"
          className="w-full rounded-sm border border-line bg-surface py-1.5 pr-2 pl-7 text-sm text-ink placeholder:text-ink-3"
        />
      </label>

      <div className="flex rounded-sm border border-line">
        {(
          [
            ["", "All"],
            ["image", "Photos"],
            ["video", "Videos"],
          ] as const
        ).map(([value, label], i) => (
          <button
            key={value}
            type="button"
            onClick={() => onKindChange(value)}
            aria-pressed={kind === value}
            className={`px-2.5 py-1.5 text-xs font-medium transition ${i > 0 ? "border-l border-line" : ""} ${
              kind === value ? "bg-accent-soft text-ink" : "text-ink-2 hover:bg-surface-2"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      <p className="tabular text-xs text-ink-3">
        {count}
        {hasMore ? "+" : ""} item{count === 1 && !hasMore ? "" : "s"}
      </p>

      <div className="flex-1" />

      {!trash && <UploadButton {...(folderId !== undefined ? { folderId } : {})} />}
    </div>
  );
}

interface SelectionBarProps {
  count: number;
  trash: boolean;
  busy: boolean;
  onClear: () => void;
  onSelectAll: () => void;
  onDelete: () => void;
}

/**
 * Replaces the toolbar while a selection is active, so the destructive action
 * only exists when there is something selected to apply it to.
 */
export function SelectionBar({
  count,
  trash,
  busy,
  onClear,
  onSelectAll,
  onDelete,
}: SelectionBarProps) {
  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-line bg-accent-soft px-3 py-2">
      <p className="tabular text-sm font-semibold text-ink">{count} selected</p>

      <button
        type="button"
        onClick={onSelectAll}
        className="rounded-sm px-2 py-1 text-xs text-ink-2 hover:text-ink"
      >
        Select all loaded
      </button>
      <button
        type="button"
        onClick={onClear}
        className="rounded-sm px-2 py-1 text-xs text-ink-2 hover:text-ink"
      >
        Clear
      </button>

      <div className="flex-1" />

      {!trash && (
        <button
          type="button"
          onClick={onDelete}
          disabled={busy}
          className="rounded-sm bg-danger-soft px-3 py-1.5 text-sm font-semibold text-danger transition hover:brightness-105 disabled:opacity-55"
        >
          {busy ? "Deleting…" : `Delete ${count}`}
        </button>
      )}
    </div>
  );
}
