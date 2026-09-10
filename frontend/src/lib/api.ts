import type {
  FileItem,
  FileListPage,
  Folder,
  Health,
  Me,
  Session,
  Share,
  StorageUsage,
  UploadInit,
} from "./types";

// One typed wrapper over fetch. Notably absent: any token handling. The session
// lives in an httpOnly cookie the browser attaches on its own, so there is
// nothing here for an XSS to read or steal (spec §8).

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }

  get isUnauthenticated(): boolean {
    return this.status === 401;
  }

  /** A retry could plausibly work: the far side is unavailable, not the request wrong. */
  get isTransient(): boolean {
    return this.status === 502 || this.status === 503 || this.status === 504;
  }
}

interface ErrorEnvelope {
  error?: { code?: string; message?: string };
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const init: RequestInit = {
    method,
    // The cookie is same-origin in production and behind the Vite proxy in
    // development, so this never becomes a cross-site credential.
    credentials: "same-origin",
    headers: { Accept: "application/json" },
    signal: signal ?? null,
  };

  if (body !== undefined) {
    init.headers = { ...init.headers, "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }

  let response: Response;
  try {
    response = await fetch(path, init);
  } catch (cause) {
    if ((cause as Error)?.name === "AbortError") throw cause;
    throw new ApiError(0, "offline", "Cannot reach the server. Check your connection.");
  }

  if (response.status === 204) return undefined as T;

  const text = await response.text();
  let parsed: unknown = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      // A non-JSON body means something upstream answered instead of the API —
      // a proxy error page, most likely. Say that rather than "unexpected token".
      throw new ApiError(response.status, "bad_response", "The server sent an unreadable response.");
    }
  }

  if (!response.ok) {
    const envelope = parsed as ErrorEnvelope | null;
    throw new ApiError(
      response.status,
      envelope?.error?.code ?? "error",
      envelope?.error?.message ?? `Request failed (${response.status}).`,
    );
  }

  return parsed as T;
}

function query(params: Record<string, string | number | boolean | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== "") search.set(key, String(value));
  }
  const encoded = search.toString();
  return encoded ? `?${encoded}` : "";
}

export const api = {
  // ---- auth
  me: () => request<Me>("GET", "/api/auth/me"),
  login: (username: string, password: string) =>
    request<Me>("POST", "/api/auth/login", { username, password }),
  logout: () => request<{ ok: boolean }>("POST", "/api/auth/logout"),
  changePassword: (current: string, next: string) =>
    request<{ ok: boolean }>("POST", "/api/auth/password", {
      current_password: current,
      new_password: next,
    }),

  // ---- files
  listFiles: (params: {
    folderId?: number | "root";
    kind?: string;
    q?: string;
    cursor?: string;
    limit?: number;
    deleted?: boolean;
  }) =>
    request<FileListPage>(
      "GET",
      `/api/files${query({
        folder_id: params.folderId,
        kind: params.kind,
        q: params.q,
        cursor: params.cursor,
        limit: params.limit,
        deleted: params.deleted ? 1 : undefined,
      })}`,
    ),
  getFile: (id: number) => request<FileItem>("GET", `/api/files/${id}`),
  renameFile: (id: number, filename: string) =>
    request<FileItem>("PATCH", `/api/files/${id}`, { filename }),
  moveFile: (id: number, folderId: number | null) =>
    request<FileItem>(
      "PATCH",
      `/api/files/${id}`,
      folderId === null ? { to_root: true } : { folder_id: folderId },
    ),
  deleteFile: (id: number) => request<{ deleted: number }>("DELETE", `/api/files/${id}`),
  bulkDelete: (ids: number[]) =>
    request<{ deleted: number }>("POST", "/api/files/bulk-delete", { ids }),
  restoreFile: (id: number) => request<{ restored: number }>("POST", `/api/files/${id}/restore`),

  // ---- upload (wired up in Step 5)
  initUpload: (body: {
    filename: string;
    size: number;
    mime_type: string;
    folder_id?: number;
    sha1?: string;
    taken_at?: number;
  }) => request<UploadInit>("POST", "/api/files/init", body),
  initParts: (fileId: number, largeFileId: string, count: number) =>
    request<{ urls: { upload_url: string; upload_token: string }[] }>(
      "POST",
      "/api/files/init-parts",
      { file_id: fileId, large_file_id: largeFileId, count },
    ),
  completeUpload: (id: number, body: Record<string, unknown>) =>
    request<FileItem>("POST", `/api/files/${id}/complete`, body),
  abortUpload: (id: number, largeFileId?: string) =>
    request<{ ok: boolean }>("POST", `/api/files/${id}/abort${query({ large_file_id: largeFileId })}`),

  // ---- folders
  listFolders: () => request<{ items: Folder[] }>("GET", "/api/folders"),
  createFolder: (name: string) => request<Folder>("POST", "/api/folders", { name }),
  renameFolder: (id: number, name: string) =>
    request<Folder>("PATCH", `/api/folders/${id}`, { name }),
  deleteFolder: (id: number, mode?: "cascade" | "move_to_root") =>
    request<Record<string, unknown>>(
      "DELETE",
      `/api/folders/${id}${
        mode === "cascade" ? "?cascade=1" : mode === "move_to_root" ? "?move_to=root" : ""
      }`,
    ),

  // ---- shares
  listShares: () => request<{ items: Share[] }>("GET", "/api/shares"),
  createShare: (fileId: number, expiresInDays: number) =>
    request<Share>("POST", "/api/shares", { file_id: fileId, expires_in_days: expiresInDays }),
  revokeShare: (id: number) => request<{ revoked: boolean }>("DELETE", `/api/shares/${id}`),

  // ---- sessions
  listSessions: () => request<{ items: Session[] }>("GET", "/api/sessions"),
  revokeSession: (id: string) =>
    request<{ revoked: boolean; signed_out: boolean }>("DELETE", `/api/sessions/${id}`),
  revokeOtherSessions: () =>
    request<{ revoked: number }>("POST", "/api/sessions/revoke-others"),

  // ---- misc
  usage: () => request<StorageUsage>("GET", "/api/storage/usage"),
  health: () => request<Health>("GET", "/api/health"),
};
