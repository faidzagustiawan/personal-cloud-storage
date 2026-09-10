import { useVirtualizer } from "@tanstack/react-virtual";
import { useEffect, useLayoutEffect, useRef, useState } from "react";

import type { FileItem } from "../../lib/types";
import { Spinner } from "../ui";
import { Tile, TILE_CAPTION_HEIGHT, TILE_GAP, TILE_MIN_WIDTH } from "./Tile";

interface VirtualGridProps {
  files: FileItem[];
  selected: Set<number>;
  onOpen: (index: number) => void;
  onToggle: (index: number, event: React.MouseEvent) => void;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
}

/**
 * A windowed grid.
 *
 * Only the rows near the viewport are in the DOM, so a library of tens of
 * thousands of photos costs the same as a library of fifty. Rows are the unit
 * of virtualization, and the column count is measured rather than assumed so
 * the grid reflows with the sidebar and the phone breakpoint.
 */
export function VirtualGrid({
  files,
  selected,
  onOpen,
  onToggle,
  hasMore,
  loadingMore,
  onLoadMore,
}: VirtualGridProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [columns, setColumns] = useState(1);
  const [tileHeight, setTileHeight] = useState(TILE_MIN_WIDTH + TILE_CAPTION_HEIGHT);

  useLayoutEffect(() => {
    const element = scrollRef.current;
    if (!element) return;

    const measure = () => {
      const width = element.clientWidth - 2 * TILE_GAP;
      if (width <= 0) return;
      const count = Math.max(1, Math.floor((width + TILE_GAP) / (TILE_MIN_WIDTH + TILE_GAP)));
      const tileWidth = (width - (count - 1) * TILE_GAP) / count;
      setColumns(count);
      // The thumbnail is square and the caption is a fixed two lines, so the
      // row height follows from the measured tile width.
      setTileHeight(tileWidth + TILE_CAPTION_HEIGHT);
    };

    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  const rowCount = Math.ceil(files.length / columns);
  const virtualizer = useVirtualizer({
    count: rowCount,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => tileHeight + TILE_GAP,
    overscan: 3,
  });

  // Re-measure when the row height changes: the virtualizer caches sizes and
  // would otherwise keep positioning rows against the old estimate.
  useEffect(() => {
    virtualizer.measure();
  }, [tileHeight, columns, virtualizer]);

  const rows = virtualizer.getVirtualItems();
  const lastRow = rows[rows.length - 1];

  useEffect(() => {
    if (!hasMore || loadingMore || !lastRow) return;
    // Fetch a page ahead of the viewport so scrolling does not stall at the
    // boundary waiting on a round trip.
    if (lastRow.index >= rowCount - 3) onLoadMore();
  }, [hasMore, loadingMore, lastRow, rowCount, onLoadMore]);

  return (
    <div ref={scrollRef} className="min-h-0 flex-1 overflow-y-auto overscroll-contain">
      <div
        style={{ height: virtualizer.getTotalSize(), position: "relative" }}
        className="px-2.5 pt-2.5"
      >
        {rows.map((row) => {
          const start = row.index * columns;
          const rowFiles = files.slice(start, start + columns);

          return (
            <div
              key={row.key}
              style={{
                position: "absolute",
                top: 0,
                left: 0,
                width: "100%",
                height: tileHeight,
                transform: `translateY(${row.start}px)`,
                display: "grid",
                gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))`,
                gap: TILE_GAP,
                paddingInline: TILE_GAP,
              }}
            >
              {rowFiles.map((file, offset) => {
                const index = start + offset;
                return (
                  <Tile
                    key={file.id}
                    file={file}
                    selected={selected.has(file.id)}
                    selecting={selected.size > 0}
                    onOpen={() => onOpen(index)}
                    onToggle={(event) => onToggle(index, event)}
                  />
                );
              })}
            </div>
          );
        })}
      </div>

      {loadingMore && (
        <div className="flex justify-center py-5">
          <Spinner className="text-ink-3" />
        </div>
      )}
      {!hasMore && files.length > 0 && (
        <p className="tabular py-5 text-center text-[11px] text-ink-3">
          {files.length} item{files.length === 1 ? "" : "s"} · end of library
        </p>
      )}
    </div>
  );
}
