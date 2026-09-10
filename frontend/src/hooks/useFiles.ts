import {
  useInfiniteQuery,
  useMutation,
  useQueryClient,
  type InfiniteData,
} from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../lib/api";
import type { FileItem, FileListPage } from "../lib/types";
import { keys } from "./queries";

export interface GalleryScope {
  folderId?: number;
  trash?: boolean;
  kind?: "image" | "video";
  query?: string;
}

/**
 * Pages the gallery with the server's keyset cursor.
 *
 * Not offset pagination: page N of an offset query has to walk N*limit rows and
 * shifts under concurrent inserts, which in a photo library means a row
 * appearing twice or not at all while an import is running.
 */
export function useFileList(scope: GalleryScope) {
  const query = useInfiniteQuery({
    queryKey: keys.files(scope as Record<string, unknown>),
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      api.listFiles({
        ...(scope.folderId !== undefined ? { folderId: scope.folderId } : {}),
        ...(scope.trash ? { deleted: true } : {}),
        ...(scope.kind ? { kind: scope.kind } : {}),
        ...(scope.query ? { q: scope.query } : {}),
        ...(pageParam ? { cursor: pageParam as string } : {}),
        limit: 60,
      }),
    getNextPageParam: (last: FileListPage) => last.next_cursor || undefined,
  });

  const items = useMemo(
    () => (query.data as InfiniteData<FileListPage> | undefined)?.pages.flatMap((p) => p.items ?? []) ?? [],
    [query.data],
  );

  return { ...query, items };
}

/** Invalidating every file list at once keeps folder counts and the trash in step. */
function refreshAll(client: ReturnType<typeof useQueryClient>) {
  client.invalidateQueries({ queryKey: ["files"] });
  client.invalidateQueries({ queryKey: keys.usage });
  client.invalidateQueries({ queryKey: keys.folders });
}

export function useDeleteFiles() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (ids: number[]) =>
      ids.length === 1 ? api.deleteFile(ids[0]!) : api.bulkDelete(ids),
    onSuccess: () => refreshAll(client),
  });
}

export function useRestoreFile() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.restoreFile(id),
    onSuccess: () => refreshAll(client),
  });
}

export function useRenameFile() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, filename }: { id: number; filename: string }) =>
      api.renameFile(id, filename),
    onSuccess: () => refreshAll(client),
  });
}

export function useMoveFile() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, folderId }: { id: number; folderId: number | null }) =>
      api.moveFile(id, folderId),
    onSuccess: () => refreshAll(client),
  });
}

export function useRenameFolder() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, name }: { id: number; name: string }) => api.renameFolder(id, name),
    onSuccess: () => refreshAll(client),
  });
}

export function useDeleteFolder() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, mode }: { id: number; mode?: "cascade" | "move_to_root" }) =>
      api.deleteFolder(id, mode),
    onSuccess: () => refreshAll(client),
  });
}

/** True when this browser has no decoder for the file's codec (spec §5.4). */
export function canPlay(file: FileItem): boolean {
  if (file.kind !== "video") return true;
  const probe = document.createElement("video");
  // canPlayType is conservative — it answers "" for video/quicktime even where
  // Chrome plays H.264 inside a .MOV perfectly well — so a negative here is
  // treated as "probably not" and the viewer offers a download, rather than as
  // a hard refusal to try.
  return probe.canPlayType(file.mime_type) !== "";
}
