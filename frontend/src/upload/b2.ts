import type { UploadTarget } from "../lib/types";

/**
 * Direct-to-B2 transfer. These are the only requests in the app that do not go
 * to our own server — decision D1 is what keeps 500 MB of video off the VPS.
 *
 * XMLHttpRequest rather than fetch, because fetch still cannot report upload
 * progress, and a 400 MB upload with no progress bar is indistinguishable from
 * a hang.
 */

export const PART_SIZE = 10 << 20; // 10 MB, spec §5.3
export const LARGE_FILE_CUTOFF = 100 << 20;
export const PART_CONCURRENCY = 4;

export class B2Error extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "B2Error";
    this.status = status;
  }
  /** 401 means the upload token went stale, which is normal and recoverable. */
  get needsFreshTarget(): boolean {
    return this.status === 401 || this.status === 408 || this.status === 429 || this.status >= 500;
  }
}

export interface UploadFileResult {
  fileId: string;
  contentSha1: string;
}

/** Percent-encodes each segment but leaves the "/" separators literal, as B2 requires. */
export function escapeName(name: string): string {
  return name.split("/").map(encodeURIComponent).join("/");
}

export async function sha1Hex(blob: Blob): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-1", await blob.arrayBuffer());
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

interface SendOptions {
  target: UploadTarget;
  headers: Record<string, string>;
  body: Blob;
  signal: AbortSignal;
  onProgress?: (bytesSent: number) => void;
}

function send({ target, headers, body, signal, onProgress }: SendOptions): Promise<unknown> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException("aborted", "AbortError"));
      return;
    }

    const xhr = new XMLHttpRequest();
    xhr.open("POST", target.upload_url, true);
    xhr.setRequestHeader("Authorization", target.upload_token);
    for (const [key, value] of Object.entries(headers)) xhr.setRequestHeader(key, value);

    const onAbort = () => xhr.abort();
    signal.addEventListener("abort", onAbort, { once: true });
    const cleanup = () => signal.removeEventListener("abort", onAbort);

    if (onProgress) {
      xhr.upload.onprogress = (event) => onProgress(event.loaded);
    }
    xhr.onload = () => {
      cleanup();
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          resolve(JSON.parse(xhr.responseText));
        } catch {
          resolve({});
        }
        return;
      }
      let message = `B2 returned ${xhr.status}`;
      try {
        const parsed = JSON.parse(xhr.responseText) as { message?: string; code?: string };
        if (parsed.message) message = `${parsed.code ?? "error"}: ${parsed.message}`;
      } catch {
        // A non-JSON body from B2 means an edge or proxy answered instead.
      }
      reject(new B2Error(xhr.status, message));
    };
    xhr.onerror = () => {
      cleanup();
      // A network-level failure here is usually a missing CORS rule on the
      // bucket (spec §2.2), which the browser reports as an opaque error.
      reject(new B2Error(0, "Could not reach storage. Check the bucket CORS rules."));
    };
    xhr.onabort = () => {
      cleanup();
      reject(new DOMException("aborted", "AbortError"));
    };

    xhr.send(body);
  });
}

async function withRetry<T>(
  attempt: (isRetry: boolean) => Promise<T>,
  refresh: () => Promise<void>,
  signal: AbortSignal,
): Promise<T> {
  let lastError: unknown;
  for (let tries = 0; tries < 3; tries++) {
    try {
      return await attempt(tries > 0);
    } catch (error) {
      if (signal.aborted || (error as Error)?.name === "AbortError") throw error;
      lastError = error;
      if (error instanceof B2Error && !error.needsFreshTarget) throw error;
      await delay(2 ** tries * 1000, signal);
      await refresh();
    }
  }
  throw lastError;
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = window.setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      window.clearTimeout(timer);
      reject(new DOMException("aborted", "AbortError"));
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

// ---------------------------------------------------------------- single file

export async function uploadSmallFile(options: {
  target: UploadTarget;
  refreshTarget: () => Promise<UploadTarget>;
  name: string;
  contentType: string;
  body: Blob;
  sha1: string;
  signal: AbortSignal;
  onProgress?: (bytesSent: number) => void;
}): Promise<UploadFileResult> {
  let target = options.target;

  const response = (await withRetry(
    () =>
      send({
        target,
        headers: {
          "X-Bz-File-Name": escapeName(options.name),
          "Content-Type": options.contentType,
          "X-Bz-Content-Sha1": options.sha1,
        },
        body: options.body,
        signal: options.signal,
        ...(options.onProgress ? { onProgress: options.onProgress } : {}),
      }),
    async () => {
      target = await options.refreshTarget();
    },
    options.signal,
  )) as { fileId?: string; contentSha1?: string };

  return { fileId: response.fileId ?? "", contentSha1: response.contentSha1 ?? options.sha1 };
}

// ---------------------------------------------------------------- multipart

export interface MultipartOptions {
  body: Blob;
  /** One target per concurrent worker: a B2 upload URL serves one upload at a time. */
  targets: UploadTarget[];
  refreshTarget: () => Promise<UploadTarget>;
  signal: AbortSignal;
  onProgress?: (bytesSent: number) => void;
}

/**
 * Uploads a large file as 10 MB parts, four in flight.
 *
 * Peak memory stays near four parts regardless of file size, and a failed part
 * costs 10 MB of re-upload rather than the whole file — which is the entire
 * reason for using the multipart API at a 500 MB ceiling.
 */
export async function uploadParts(options: MultipartOptions): Promise<string[]> {
  const { body, signal } = options;
  const partCount = Math.ceil(body.size / PART_SIZE);
  const sha1s = new Array<string>(partCount);
  const sentPerPart = new Array<number>(partCount).fill(0);

  const report = () => {
    options.onProgress?.(sentPerPart.reduce((total, n) => total + n, 0));
  };

  let next = 0;
  const claim = () => (next < partCount ? next++ : -1);

  const workers = options.targets.map(async (initialTarget) => {
    let target = initialTarget;

    for (let index = claim(); index !== -1; index = claim()) {
      const start = index * PART_SIZE;
      const chunk = body.slice(start, Math.min(start + PART_SIZE, body.size));
      const digest = await sha1Hex(chunk);
      sha1s[index] = digest;

      await withRetry(
        (isRetry) => {
          if (isRetry) {
            // A retried part starts its byte count over, or the total drifts up.
            sentPerPart[index] = 0;
            report();
          }
          return send({
            target,
            headers: {
              "X-Bz-Part-Number": String(index + 1),
              "X-Bz-Content-Sha1": digest,
            },
            body: chunk,
            signal,
            onProgress: (loaded) => {
              sentPerPart[index] = loaded;
              report();
            },
          });
        },
        async () => {
          target = await options.refreshTarget();
        },
        signal,
      );

      sentPerPart[index] = chunk.size;
      report();
    }
  });

  await Promise.all(workers);
  return sha1s;
}
