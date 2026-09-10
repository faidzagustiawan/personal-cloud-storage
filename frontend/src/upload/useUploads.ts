import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useSyncExternalStore } from "react";

import { keys } from "../hooks/queries";
import { uploadQueue, type UploadTask } from "./queue";

/** Subscribes to the queue without re-rendering on every progress tick. */
export function useUploadTasks(): UploadTask[] {
  return useSyncExternalStore(uploadQueue.subscribe, uploadQueue.getSnapshot, uploadQueue.getSnapshot);
}

/**
 * Refreshes the gallery when uploads commit.
 *
 * Debounced, because a fifty-photo import would otherwise refetch the file list
 * fifty times — once per commit — while the remaining uploads are still running.
 */
export function useUploadRefresh(): void {
  const client = useQueryClient();

  useEffect(() => {
    let timer: number | undefined;

    uploadQueue.onCommitted = () => {
      window.clearTimeout(timer);
      timer = window.setTimeout(() => {
        client.invalidateQueries({ queryKey: ["files"] });
        client.invalidateQueries({ queryKey: keys.usage });
        client.invalidateQueries({ queryKey: keys.folders });
      }, 800);
    };

    return () => {
      window.clearTimeout(timer);
      uploadQueue.onCommitted = null;
    };
  }, [client]);
}
