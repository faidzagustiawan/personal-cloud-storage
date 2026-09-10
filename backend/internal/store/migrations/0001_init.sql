-- Initial schema. Mirrors CLOUD_STORAGE_SPEC.md §3 exactly.
--
-- Every timestamp is unix seconds stored as INTEGER. SQLite has no real date
-- type, and mixing formats across tables produces comparison bugs that only
-- surface once there is real data.

CREATE TABLE users (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  username        TEXT    NOT NULL UNIQUE,
  password_hash   TEXT    NOT NULL,                        -- bcrypt, cost 12
  storage_used    INTEGER NOT NULL DEFAULT 0,              -- bytes; only ever changed in the same tx as a files row
  storage_quota   INTEGER NOT NULL DEFAULT 107374182400,   -- 100 GB hard cap (decision D9)
  created_at      INTEGER NOT NULL,
  last_login_at   INTEGER
);

CREATE TABLE sessions (
  token_hash      TEXT    PRIMARY KEY,                     -- sha256 of the opaque token; the raw token is never stored
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  revoked_at      INTEGER,
  user_agent      TEXT
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);

CREATE TABLE folders (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name              TEXT    NOT NULL,
  parent_folder_id  INTEGER REFERENCES folders(id),        -- always NULL in v1 (decision D5)
  created_at        INTEGER NOT NULL,
  UNIQUE(user_id, parent_folder_id, name)
);

CREATE TABLE files (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  folder_id       INTEGER REFERENCES folders(id),

  object_key      TEXT    NOT NULL UNIQUE,                 -- 32 hex chars, crypto/rand, generated before insert
  filename        TEXT    NOT NULL,                        -- display name, user-editable, NOT the storage key
  ext             TEXT    NOT NULL,
  mime_type       TEXT    NOT NULL,
  kind            TEXT    NOT NULL CHECK (kind IN ('image','video')),
  file_size       INTEGER NOT NULL,
  sha1            TEXT,                                    -- B2 requires it anyway; gives dedup + integrity free

  b2_file_id      TEXT,                                    -- needed for b2_delete_file_version
  has_thumb       INTEGER NOT NULL DEFAULT 0,
  thumb_status    TEXT    NOT NULL DEFAULT 'pending'
                  CHECK (thumb_status IN ('pending','ready','failed','unsupported')),
  thumb_tier      TEXT,                                    -- 'native' | 'heic2any' | 'dng-preview' | 'ffmpeg'
  thumb_format    TEXT,                                    -- blob type actually produced; see §5.4
  thumb_attempts  INTEGER NOT NULL DEFAULT 0,              -- tier 3 gives up at 3

  width           INTEGER,
  height          INTEGER,
  duration_sec    REAL,
  taken_at        INTEGER,                                 -- EXIF DateTimeOriginal when present, else upload time

  status          TEXT    NOT NULL DEFAULT 'uploading'
                  CHECK (status IN ('uploading','ready')),
  uploaded_at     INTEGER NOT NULL,
  is_deleted      INTEGER NOT NULL DEFAULT 0,
  deleted_at      INTEGER                                  -- purged from B2 30 days after this
);

CREATE INDEX idx_files_gallery   ON files(user_id, is_deleted, status, taken_at DESC);
CREATE INDEX idx_files_folder    ON files(user_id, folder_id, is_deleted);
CREATE INDEX idx_files_sha1      ON files(user_id, sha1);
CREATE INDEX idx_files_purge     ON files(is_deleted, deleted_at) WHERE is_deleted = 1;
CREATE INDEX idx_files_orphan    ON files(status, uploaded_at) WHERE status = 'uploading';
CREATE INDEX idx_files_thumbjob  ON files(thumb_status, thumb_attempts) WHERE thumb_status = 'unsupported';

CREATE TABLE shares (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  file_id         INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  token           TEXT    NOT NULL UNIQUE,                 -- 32 bytes crypto/rand, base64url (256-bit)
  expires_at      INTEGER NOT NULL,
  revoked_at      INTEGER,
  view_count      INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_shares_token ON shares(token);

-- Cached B2 credentials, so a restart does not re-spend Class C transactions (§2.4).
CREATE TABLE b2_tokens (
  scope           TEXT PRIMARY KEY,                        -- e.g. 'download:users/1/'
  token           TEXT NOT NULL,
  expires_at      INTEGER NOT NULL
);
