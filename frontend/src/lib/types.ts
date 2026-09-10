// Shapes returned by the Go API. Kept hand-written rather than generated:
// there are a dozen of them, and a mismatch shows up immediately in a typed
// component rather than being silently smoothed over by `any`.

export type Kind = "image" | "video";

export type ThumbStatus = "pending" | "ready" | "failed" | "unsupported";

export interface Me {
  username: string;
  storage_used: number;
  storage_quota: number;
  last_login_at?: number;
}

export interface FileItem {
  id: number;
  filename: string;
  kind: Kind;
  mime_type: string;
  file_size: number;
  taken_at: number;
  uploaded_at: number;
  thumb_status: ThumbStatus;
  status: "uploading" | "ready";
  orig_url: string;
  /** Absent when no thumbnail was produced; the grid shows a placeholder. */
  thumb_url?: string;
  folder_id?: number;
  width?: number;
  height?: number;
  duration_sec?: number;
  is_deleted?: boolean;
  deleted_at?: number;
}

export interface FileListPage {
  items: FileItem[];
  next_cursor: string;
}

export interface Folder {
  id: number;
  name: string;
  file_count: number;
  created_at: number;
}

export interface StorageUsage {
  used_bytes: number;
  quota_bytes: number;
  usage_percent: number;
  image_bytes: number;
  video_bytes: number;
  trash_bytes: number;
  file_count: number;
  thumbnails_pending: number;
}

export interface Health {
  status: "ok" | "incomplete" | "degraded";
  version: string;
  db_ok: boolean;
  b2: {
    state: "unconfigured" | "ok" | "error";
    detail?: string;
    upload_key_scope?: string;
  };
  users: number;
  uptime_sec: number;
}

export interface Share {
  id: number;
  file_id: number;
  filename: string;
  kind: Kind;
  /** The full public link, already built against the server's canonical origin. */
  url: string;
  expires_at: number;
  created_at: number;
  view_count: number;
  active: boolean;
  revoked_at?: number;
}

export interface Session {
  /** SHA-256 of the session token — a digest, not a credential. */
  id: string;
  created_at: number;
  expires_at: number;
  user_agent: string;
  current: boolean;
}

/** Credentials for one direct-to-B2 upload. Used from Step 5 onward. */
export interface UploadTarget {
  upload_url: string;
  upload_token: string;
  file_id?: string;
}

export interface UploadInit {
  file_id: number;
  object_key: string;
  orig_name: string;
  thumb_name: string;
  part_size: number;
  multipart: boolean;
  large_file_id?: string;
  orig?: UploadTarget;
  thumb?: UploadTarget;
}
