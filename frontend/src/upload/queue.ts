import { api, ApiError } from "../lib/api";
import type { UploadTarget } from "../lib/types";
import {
  LARGE_FILE_CUTOFF,
  PART_CONCURRENCY,
  sha1Hex,
  uploadParts,
  uploadSmallFile,
} from "./b2";
import { buildThumbnail } from "./thumbnail";
import { kindOf, resolveMimeType, sniffContainer } from "./sniff";

/**
 * Drives uploads from picked file to committed row.
 *
 * Per file: sniff, hash, reserve, then push the bytes straight to B2 while the
 * thumbnail is built in parallel, then commit. The server never sees a byte —
 * it hands out short-lived credentials and verifies the result afterwards
 * (spec §4.2).
 */

export const FILE_CONCURRENCY = 2; // enough to keep the link busy, not enough to saturate it
export const MAX_FILE_SIZE = 500 << 20;

export type TaskState =
  | "queued"
  | "preparing"
  | "uploading"
  | "finishing"
  | "done"
  | "error"
  | "cancelled";

export interface UploadTask {
  id: string;
  name: string;
  size: number;
  state: TaskState;
  /** Bytes of the original transferred so far. */
  sent: number;
  error?: string;
  /** Set once the thumbnail path is known, for the panel's detail line. */
  tier?: string;
  /** True when no thumbnail could be built and the server worker will try. */
  thumbnailQueued?: boolean;
  fileId?: number;
}

type Listener = () => void;

interface Job {
  task: UploadTask;
  file: File;
  folderId: number | undefined;
  controller: AbortController;
}

export class UploadQueue {
  private jobs = new Map<string, Job>();
  private order: string[] = [];
  private running = 0;
  private listeners = new Set<Listener>();
  private snapshot: UploadTask[] = [];
  private notifyScheduled = false;
  private flushTimer: number | undefined;

  /** Called after each successful commit so the gallery can refresh. */
  onCommitted: ((fileId: number) => void) | null = null;

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): UploadTask[] => this.snapshot;

  add(files: File[], folderId?: number): void {
    for (const file of files) {
      const id = `${Date.now()}-${Math.random().toString(36).slice(2, 9)}`;
      const task: UploadTask = {
        id,
        name: file.name,
        size: file.size,
        state: "queued",
        sent: 0,
      };

      if (file.size === 0) {
        this.jobs.set(id, { task: { ...task, state: "error", error: "This file is empty." }, file, folderId, controller: new AbortController() });
        this.order.push(id);
        continue;
      }
      if (file.size > MAX_FILE_SIZE) {
        task.state = "error";
        task.error = "Larger than the 500 MB limit.";
      }

      this.jobs.set(id, { task, file, folderId, controller: new AbortController() });
      this.order.push(id);
    }
    this.publish();
    this.pump();
  }

  cancel(id: string): void {
    const job = this.jobs.get(id);
    if (!job) return;
    job.controller.abort();
    if (job.task.state === "queued") this.update(id, { state: "cancelled" });
  }

  /** Drops finished rows from the panel. Running uploads are left alone. */
  clearFinished(): void {
    for (const [id, job] of this.jobs) {
      if (["done", "error", "cancelled"].includes(job.task.state)) {
        this.jobs.delete(id);
        this.order = this.order.filter((other) => other !== id);
      }
    }
    this.publish();
  }

  // ---------------------------------------------------------------- internals

  private update(id: string, patch: Partial<UploadTask>): void {
    const job = this.jobs.get(id);
    if (!job) return;
    job.task = { ...job.task, ...patch };
    // A state change is news; a progress tick is not. Flushing transitions at
    // once keeps "Failed" and "Done" from waiting behind a throttle window.
    this.publish(patch.state !== undefined || patch.error !== undefined);
  }

  /**
   * Progress events fire many times a second per part, so they are coalesced.
   *
   * Deliberately a timer and not requestAnimationFrame: rAF does not run while
   * the tab is hidden, so switching away mid-upload would freeze every update —
   * including the final Done or Failed — until the user came back. A timer keeps
   * the state correct whether or not anything is being painted.
   */
  private publish(immediate = false): void {
    if (immediate) {
      window.clearTimeout(this.flushTimer);
      this.notifyScheduled = false;
      this.flush();
      return;
    }
    if (this.notifyScheduled) return;
    this.notifyScheduled = true;
    this.flushTimer = window.setTimeout(() => {
      this.notifyScheduled = false;
      this.flush();
    }, 120);
  }

  private flush(): void {
    this.snapshot = this.order
      .map((id) => this.jobs.get(id)?.task)
      .filter((task): task is UploadTask => task !== undefined);
    for (const listener of this.listeners) listener();
  }

  private pump(): void {
    while (this.running < FILE_CONCURRENCY) {
      const next = this.order
        .map((id) => this.jobs.get(id))
        .find((job) => job?.task.state === "queued");
      if (!next) return;

      this.running += 1;
      void this.run(next).finally(() => {
        this.running -= 1;
        this.pump();
      });
    }
  }

  private async run(job: Job): Promise<void> {
    const { task, file, controller } = job;
    const { signal } = controller;
    let fileId: number | undefined;
    let largeFileId: string | undefined;

    try {
      this.update(task.id, { state: "preparing" });

      const container = await sniffContainer(file);
      const mimeType = resolveMimeType(file, container);
      const kind = kindOf(mimeType);
      if (!kind) {
        throw new Error("This app stores photos and videos. That is neither.");
      }

      // A whole-file SHA-1 needs the whole file in memory, so it is computed
      // only below the multipart cutoff. Large files are verified by byte count
      // instead, which is the check that actually protects the quota; the cost
      // is that re-uploading the same large video is not deduplicated.
      const isLarge = file.size > LARGE_FILE_CUTOFF;
      const sha1 = isLarge ? "" : await sha1Hex(file);
      if (signal.aborted) throw abortError();

      const probePromise = buildThumbnail(file, container, kind);

      const init = await api.initUpload({
        filename: file.name,
        size: file.size,
        mime_type: mimeType,
        ...(job.folderId !== undefined ? { folder_id: job.folderId } : {}),
        ...(sha1 ? { sha1 } : {}),
        taken_at: Math.floor(file.lastModified / 1000),
      });
      fileId = init.file_id;
      largeFileId = init.large_file_id;
      this.update(task.id, { state: "uploading", fileId });

      let b2FileId = "";
      let partSha1s: string[] = [];

      if (init.multipart && init.large_file_id) {
        const { urls } = await api.initParts(init.file_id, init.large_file_id, PART_CONCURRENCY);
        partSha1s = await uploadParts({
          body: file,
          targets: urls,
          refreshTarget: async () => {
            const fresh = await api.initParts(init.file_id, init.large_file_id!, 1);
            return firstTarget(fresh.urls);
          },
          signal,
          onProgress: (sent) => this.update(task.id, { sent }),
        });
      } else {
        if (!init.orig) throw new Error("The server issued no upload credentials.");
        const result = await uploadSmallFile({
          target: init.orig,
          refreshTarget: async () => {
            const fresh = await api.initUpload({
              filename: file.name,
              size: file.size,
              mime_type: mimeType,
            });
            if (!fresh.orig) throw new Error("The server issued no upload credentials.");
            return fresh.orig;
          },
          name: init.orig_name,
          contentType: mimeType,
          body: file,
          sha1,
          signal,
          onProgress: (sent) => this.update(task.id, { sent }),
        });
        b2FileId = result.fileId;
      }

      const probe = await probePromise;
      if (signal.aborted) throw abortError();

      let thumbOK = false;
      if (probe.thumbnail && init.thumb) {
        try {
          const thumbSha1 = await sha1Hex(probe.thumbnail.blob);
          await uploadSmallFile({
            target: init.thumb,
            refreshTarget: async () => init.thumb as UploadTarget,
            name: init.thumb_name,
            contentType: probe.thumbnail.format,
            body: probe.thumbnail.blob,
            sha1: thumbSha1,
            signal,
          });
          thumbOK = true;
        } catch (error) {
          // The original is already stored. A thumbnail that would not upload
          // is a cosmetic loss, and the server-side worker gets another go.
          if ((error as Error)?.name === "AbortError") throw error;
          console.warn("thumbnail upload failed", error);
        }
      }

      this.update(task.id, {
        state: "finishing",
        sent: file.size,
        ...(probe.thumbnail ? { tier: probe.thumbnail.tier } : {}),
        thumbnailQueued: !thumbOK,
      });

      const saved = await api.completeUpload(init.file_id, {
        b2_file_id: b2FileId,
        ...(init.large_file_id ? { large_file_id: init.large_file_id, part_sha1s: partSha1s } : {}),
        sha1,
        ...(probe.width !== undefined ? { width: probe.width } : {}),
        ...(probe.height !== undefined ? { height: probe.height } : {}),
        ...(probe.durationSec !== undefined ? { duration_sec: probe.durationSec } : {}),
        taken_at: probe.takenAt ?? Math.floor(file.lastModified / 1000),
        thumb_ok: thumbOK,
        ...(probe.thumbnail ? { thumb_tier: probe.thumbnail.tier, thumb_format: probe.thumbnail.format } : {}),
      });

      this.update(task.id, { state: "done", fileId: saved.id });
      this.onCommitted?.(saved.id);
    } catch (error) {
      const aborted = (error as Error)?.name === "AbortError" || signal.aborted;

      // Whatever went wrong, the reservation must not be left behind: it counts
      // against the quota until the orphan sweep runs, and B2 bills the parts of
      // an unfinished multipart upload the whole time.
      if (fileId !== undefined) {
        void api.abortUpload(fileId, largeFileId).catch(() => undefined);
      }

      this.update(task.id, {
        state: aborted ? "cancelled" : "error",
        ...(aborted ? {} : { error: describeError(error) }),
      });
    }
  }
}

function firstTarget(urls: UploadTarget[]): UploadTarget {
  const target = urls[0];
  if (!target) throw new Error("The server issued no upload credentials.");
  return target;
}

function abortError(): DOMException {
  return new DOMException("aborted", "AbortError");
}

function describeError(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.code) {
      case "quota_exceeded":
        return "Not enough storage left. Delete something first.";
      case "duplicate":
        return "Already in your library.";
      case "unsupported_type":
        return "This app stores photos and videos only.";
      case "storage_unavailable":
        return "Storage is not configured on this server yet.";
      case "verification_failed":
        return "The upload did not arrive intact. Nothing was saved.";
      default:
        return error.message;
    }
  }
  return error instanceof Error ? error.message : String(error);
}

export const uploadQueue = new UploadQueue();
