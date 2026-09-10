import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";

import { SelectionBar, Toolbar } from "../components/gallery/Toolbar";
import { VirtualGrid } from "../components/gallery/VirtualGrid";
import { EmptyState, Notice, Spinner } from "../components/ui";
import { UploadButton, UploadZone } from "../components/UploadZone";
import { Viewer } from "../components/Viewer";
import { useDeleteFiles, useFileList } from "../hooks/useFiles";
import { useFolders } from "../hooks/queries";

export function GalleryPage({ trash = false }: { trash?: boolean }) {
  const params = useParams();
  const folderId = params.folderId ? Number(params.folderId) : undefined;

  const [query, setQuery] = useState("");
  const [kind, setKind] = useState<"" | "image" | "video">("");
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [openIndex, setOpenIndex] = useState<number | null>(null);
  const lastToggled = useRef<number | null>(null);

  const folders = useFolders();
  const remove = useDeleteFiles();

  const list = useFileList({
    ...(folderId !== undefined ? { folderId } : {}),
    ...(trash ? { trash: true } : {}),
    ...(kind ? { kind } : {}),
    ...(query ? { query } : {}),
  });
  const { items } = list;

  // Changing where you are looking should not carry a selection along with it.
  useEffect(() => {
    setSelected(new Set());
    setOpenIndex(null);
  }, [folderId, trash, kind, query]);

  // Deleting from under the viewer, or a refetch that shortens the list, must
  // not leave it pointing past the end.
  useEffect(() => {
    if (openIndex !== null && openIndex >= items.length) {
      setOpenIndex(items.length > 0 ? items.length - 1 : null);
    }
  }, [items.length, openIndex]);

  const toggle = useCallback(
    (index: number, event: React.MouseEvent) => {
      const file = items[index];
      if (!file) return;

      // Read the modifier and the anchor here, not inside the updater below.
      // React runs a state updater during the render pass, by which point the
      // synthetic event has been handed back and its flags read as false —
      // which silently turns every shift-click into a plain toggle.
      const extend = event.shiftKey;
      const anchor = lastToggled.current;

      setSelected((previous) => {
        const next = new Set(previous);

        // Shift-click extends from the last toggle, which is how every file
        // manager behaves and what makes selecting a day's photos bearable.
        if (extend && anchor !== null) {
          const from = Math.min(anchor, index);
          const to = Math.max(anchor, index);
          for (let i = from; i <= to; i++) {
            const id = items[i]?.id;
            if (id !== undefined) next.add(id);
          }
          return next;
        }

        if (next.has(file.id)) next.delete(file.id);
        else next.add(file.id);
        return next;
      });

      lastToggled.current = index;
    },
    [items],
  );

  const selectAll = useCallback(() => {
    setSelected(new Set(items.map((file) => file.id)));
  }, [items]);

  const deleteSelected = useCallback(() => {
    const ids = [...selected];
    if (ids.length === 0) return;
    remove.mutate(ids, { onSuccess: () => setSelected(new Set()) });
  }, [selected, remove]);

  // Escape clears a selection; it closes the viewer on its own, because a
  // native <dialog> handles that itself.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape" && openIndex === null && selected.size > 0) {
        setSelected(new Set());
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [openIndex, selected.size]);

  const folderName = useMemo(
    () => folders.data?.find((folder) => folder.id === folderId)?.name,
    [folders.data, folderId],
  );

  const header =
    selected.size > 0 ? (
      <SelectionBar
        count={selected.size}
        trash={trash}
        busy={remove.isPending}
        onClear={() => setSelected(new Set())}
        onSelectAll={selectAll}
        onDelete={deleteSelected}
      />
    ) : (
      <Toolbar
        count={items.length}
        hasMore={list.hasNextPage}
        query={query}
        onQueryChange={setQuery}
        kind={kind}
        onKindChange={setKind}
        {...(folderId !== undefined ? { folderId } : {})}
        trash={trash}
      />
    );

  const body = (() => {
    if (list.isPending) {
      return (
        <div className="flex flex-1 items-center justify-center">
          <Spinner className="text-ink-3" />
        </div>
      );
    }
    if (list.error) {
      return (
        <div className="p-4">
          <Notice>{list.error.message}</Notice>
        </div>
      );
    }
    if (items.length === 0) {
      return <Empty trash={trash} query={query} kind={kind} folderId={folderId} />;
    }

    return (
      <VirtualGrid
        files={items}
        selected={selected}
        onOpen={setOpenIndex}
        onToggle={toggle}
        hasMore={list.hasNextPage}
        loadingMore={list.isFetchingNextPage}
        onLoadMore={list.fetchNextPage}
      />
    );
  })();

  const content = (
    <div className="flex min-h-0 flex-1 flex-col">
      {folderName && (
        <h1 className="border-b border-line px-3 pt-2.5 pb-1 text-sm font-semibold text-ink">
          {folderName}
        </h1>
      )}
      {header}
      {body}
      {remove.error && (
        <div className="p-3">
          <Notice>{remove.error.message}</Notice>
        </div>
      )}
      {openIndex !== null && items[openIndex] && (
        <Viewer
          files={items}
          index={openIndex}
          onIndexChange={setOpenIndex}
          onClose={() => setOpenIndex(null)}
          trash={trash}
        />
      )}
    </div>
  );

  // The trash is not an upload target, so it gets no drop zone.
  if (trash) return content;

  return <UploadZone {...(folderId !== undefined ? { folderId } : {})}>{content}</UploadZone>;
}

function Empty({
  trash,
  query,
  kind,
  folderId,
}: {
  trash: boolean;
  query: string;
  kind: "" | "image" | "video";
  folderId?: number;
}) {
  if (query) {
    return (
      <EmptyState
        title="Nothing matches that"
        body={`No filename contains “${query}”. Try a shorter search.`}
      />
    );
  }
  // A filter with no matches is not an empty library, and offering an upload
  // button here would suggest it is.
  if (kind) {
    return (
      <EmptyState
        title={kind === "video" ? "No videos here" : "No photos here"}
        body={`Nothing in this view is a ${kind === "video" ? "video" : "photo"}. Switch the filter back to All to see everything.`}
      />
    );
  }
  if (trash) {
    return (
      <EmptyState
        title="Trash is empty"
        body="Deleted photos stay here for 30 days, then their storage is released for good."
      />
    );
  }
  return (
    <EmptyState
      title={folderId ? "This folder is empty" : "No photos yet"}
      body="Drop photos and videos anywhere on this page, or use the Upload button."
      action={<UploadButton {...(folderId !== undefined ? { folderId } : {})} />}
    />
  );
}
