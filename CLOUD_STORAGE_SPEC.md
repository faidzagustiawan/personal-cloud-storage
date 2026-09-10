# Personal Cloud Storage — Final Specification

**Project:** Photo & video cloud storage (single user)
**Version:** 1.0 (supersedes `cloud-storage-design.md` and `CLOUD_STORAGE_DEV_PROMPT.md` — both are now historical)
**Date:** 2026-09-10
**Status:** Approved for implementation

---

## 0. GOVERNING DECISIONS

These were open or contradictory in the previous two documents. They are now closed. Do not re-litigate during implementation.

| # | Decision | Rationale |
|---|---|---|
| D1 | **Client talks to B2 directly** for upload and download. File bytes never pass through the VPS. | Explicit owner requirement: performance first, VPS load minimal. Supersedes the old "no direct B2 access from client" rule. |
| D2 | **Thumbnails are generated in the browser**, with a bounded server-side fallback for the formats no browser can decode. | Follows from D1 for the common path. But the owner's priority is that *everything* displays properly, so the rare formats get a rate-limited ffmpeg worker rather than a generic icon. See §5.4. |
| D3 | **Opaque session tokens in the database. No JWT.** | Single VPS, single user, SQLite already present. Makes logout and token revocation actually work. |
| D4 | **Subdomain `cloud.faidz.fun`**, not a `/cloud` path rewrite. | No React Router basename, no Nginx path rewriting, no cookie path scoping. |
| D5 | **Flat folders in v1.** `parent_folder_id` column exists but is always NULL. | Ships faster. Nesting becomes a v2 feature with no schema migration. |
| D6 | **systemd, not Docker.** | One static Go binary. Docker adds ~200MB and buys nothing here. |
| D7 | **Registration is disabled.** Users are created by CLI on the VPS. | An open `/register` on a personal cloud is free hosting for strangers. |
| D8 | **Cloudflare proxies all B2 downloads.** | Bandwidth Alliance makes B2 egress to Cloudflare free, and thumbnails become CDN-cached. |
| D9 | **Storage is not unlimited.** A hard quota is enforced per user and a budget alert is set at Backblaze. | "Unlimited storage" and "$0.40/month" cannot both be true. |

### Non-goals (unchanged)

Team collaboration, permissions beyond ownership, office documents, social features, comments, AI/face search, server-side video transcoding, native mobile apps.

### Known accepted limitation

A web app cannot automatically back up the phone camera roll in the background. That is the single biggest functional gap versus Google Photos, and it is accepted. Uploads are manual (multi-select from the iOS/Android share sheet or file picker).

---

## 1. ARCHITECTURE

```
                    Browser
                   /        \
   (JSON only)    /          \   (file bytes, direct)
                 /            \
                v              v
      Go API (VPS :8080)   Cloudflare  --(free egress)-->  Backblaze B2
              |             (cache)
              v
          SQLite (metadata)
```

**The VPS handles:** authentication, metadata CRUD, quota accounting, minting short-lived B2 credentials, and one HTTP redirect for share links.

**The VPS does not handle file bytes on the normal path.** Uploads, downloads and thumbnail generation all bypass it. The one exception is the fallback thumbnail worker of §5.4: one file at a time, only for formats the browser could not decode, pulling a few MB by byte-range rather than the whole object.

Expected steady-state footprint: **under 40 MB RSS**, near-zero CPU outside of request handling. The fallback worker adds a transient ~150 MB and about a second of CPU per rare file, capped at one concurrent job.

### Stack

| Layer | Choice |
|---|---|
| Backend | Go 1.22+, Gin, `modernc.org/sqlite` |
| Database | SQLite (WAL mode) |
| Storage | Backblaze B2 native API (b2 v2) |
| CDN / egress | Cloudflare (proxied CNAME to B2) |
| Frontend | React 18 + Vite + TypeScript |
| Styling | TailwindCSS |
| Data fetching | TanStack Query |
| Video playback | Native `<video>` element (H.264/HEVC). **Not Video.js** — the native element handles Range requests and seeking against B2 correctly and costs 0 KB. |

Dropped from the earlier plan: Redis (no cache layer needed — Cloudflare is the cache), GORM (use `database/sql` with explicit queries), Dropzone.js (~30 lines of native drag-and-drop), Video.js, Docker.

---

## 2. BACKBLAZE B2 SETUP

### 2.1 Bucket

- One **private** bucket, e.g. `faidz-cloud`.
- Lifecycle rule: `daysFromHidingToDeleting = 1`, `daysFromUploadingToHiding = null`. This guarantees that a hidden or superseded version stops being billed after one day. Without this rule, deleted files are billed forever.

### 2.2 CORS rules (required — direct browser upload fails without this)

```json
[{
  "corsRuleName": "browserUpload",
  "allowedOrigins": ["https://cloud.faidz.fun"],
  "allowedOperations": [
    "b2_upload_file",
    "b2_upload_part",
    "b2_download_file_by_id",
    "b2_download_file_by_name"
  ],
  "allowedHeaders": ["authorization", "content-type", "x-bz-file-name", "x-bz-content-sha1", "x-bz-part-number", "x-bz-info-*"],
  "exposeHeaders": ["x-bz-file-id", "x-bz-content-sha1"],
  "maxAgeSeconds": 3600
}]
```

### 2.3 Application keys — two, with different blast radius

| Key | Capabilities | Restriction | Lives where |
|---|---|---|---|
| **Master key** | `listFiles, readFiles, writeFiles, deleteFiles, shareFiles` | bucket-scoped | `.env` on the VPS only. Never leaves the server. |
| **Upload key** | `writeFiles` only | bucket + `namePrefix = users/1/` | `.env` on the VPS. Its *derived* short-lived upload token is handed to the browser. |

The upload token given to the browser inherits the key's restrictions, so a leaked token can only write under that user's prefix, and can never read, list, or delete. This is the security boundary that makes D1 acceptable.

### 2.4 Transaction classes and what they cost

Free tier: first 2,500 Class B + Class C transactions **per day**. Class A is always free.

| Call | Class | Frequency in this design |
|---|---|---|
| `b2_get_upload_url`, `b2_get_upload_part_url` | A (free) | per upload |
| `b2_upload_file`, `b2_upload_part`, `b2_start_large_file`, `b2_finish_large_file` | A (free) | per upload |
| `b2_delete_file_version` | A (free) | per purge |
| `b2_get_file_info` | B | **1 per completed upload** (server-side verification) |
| `b2_get_download_authorization` | C | **~1 per week** (see below) |
| `b2_authorize_account` | C | ~1 per 12h (token cached) |

**The critical cost optimization:** do **not** call `b2_get_download_authorization` per file. Call it **once per user, scoped to the prefix `users/{id}/`, with `validDurationInSeconds = 604800` (7 days, the maximum)**. Cache the returned token in memory, refresh at 6 days, persist it to SQLite so a restart does not re-spend a transaction.

The naive per-file approach costs 50 Class C transactions per gallery page load and exhausts the free tier in roughly 50 page views. The prefix approach costs one transaction per week. It also keeps the query string stable for 7 days, which is what makes Cloudflare able to cache thumbnails at all.

### 2.5 Cloudflare

1. DNS: `cdn.faidz.fun` CNAME to `f004.backblazeb2.com`, **proxied (orange cloud)** — this is what activates the Bandwidth Alliance and makes B2 egress free.
2. Transform Rule (rewrite URL path): `/f/*` becomes `/file/faidz-cloud/$1`, so object URLs stay short and do not leak the bucket name.
3. Cache Rule: for paths matching `/f/users/*/thumb/*`, set Edge TTL 1 month, Browser TTL 1 week. Thumbnails are immutable — the object key changes if the content changes.
4. Do **not** add a force-cache rule for `/f/users/*/orig/*`. Large video files fall under Cloudflare's free-plan restrictions on caching non-HTML content. Leave originals uncached; egress from B2 to Cloudflare is free either way, so the cost benefit is preserved regardless.

Final download URL shape:

```
https://cdn.faidz.fun/f/users/1/thumb/{object_key}.webp?Authorization={prefix_token}
```

---

## 3. DATA MODEL

Single authoritative schema. WAL mode and a single writer connection are mandatory — see §7.1.

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE users (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  username        TEXT    NOT NULL UNIQUE,
  password_hash   TEXT    NOT NULL,           -- bcrypt, cost 12
  storage_used    INTEGER NOT NULL DEFAULT 0, -- bytes; only changed inside the same tx as a files row change
  storage_quota   INTEGER NOT NULL DEFAULT 107374182400, -- 100 GB hard cap (see D9)
  created_at      INTEGER NOT NULL,           -- unix seconds, everywhere
  last_login_at   INTEGER
);

CREATE TABLE sessions (
  token_hash      TEXT    PRIMARY KEY,        -- sha256 of the opaque token; the raw token is never stored
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  revoked_at      INTEGER,
  user_agent      TEXT
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE folders (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name              TEXT    NOT NULL,
  parent_folder_id  INTEGER REFERENCES folders(id),  -- always NULL in v1 (D5)
  created_at        INTEGER NOT NULL
);

-- The obvious UNIQUE(user_id, parent_folder_id, name) on the table does not
-- work here: SQLite treats NULLs as distinct from one another in a UNIQUE
-- index, and parent_folder_id is always NULL while folders are flat (D5), so
-- every root folder would look unique regardless of name. COALESCE folds the
-- NULL into a real value so the index bites; lower() makes it case-insensitive,
-- because "Trips" beside "trips" in the sidebar is a bug report waiting to
-- happen. The expression keeps working unchanged when nesting arrives in v2.
CREATE UNIQUE INDEX idx_folders_unique_name
  ON folders(user_id, COALESCE(parent_folder_id, 0), lower(name));

CREATE TABLE files (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  folder_id       INTEGER REFERENCES folders(id),

  object_key      TEXT    NOT NULL UNIQUE,   -- 32 hex chars, crypto/rand. Unguessable, generated before insert.
  filename        TEXT    NOT NULL,          -- display name, user-editable, NOT the storage key
  ext             TEXT    NOT NULL,
  mime_type       TEXT    NOT NULL,
  kind            TEXT    NOT NULL CHECK (kind IN ('image','video')),
  file_size       INTEGER NOT NULL,
  sha1            TEXT,                      -- B2 requires it anyway; gives dedup + integrity for free

  b2_file_id      TEXT,                      -- needed for b2_delete_file_version
  has_thumb       INTEGER NOT NULL DEFAULT 0,
  thumb_status    TEXT    NOT NULL DEFAULT 'pending'
                  CHECK (thumb_status IN ('pending','ready','failed','unsupported')),
  thumb_tier      TEXT,                      -- 'native' | 'heic2any' | 'dng-preview' | 'ffmpeg'
  thumb_format    TEXT,                      -- the blob type actually produced; see §5.4
  thumb_attempts  INTEGER NOT NULL DEFAULT 0, -- Tier 3 gives up at 3

  width           INTEGER,
  height          INTEGER,
  duration_sec    REAL,
  taken_at        INTEGER,                   -- from EXIF DateTimeOriginal when present, else upload time

  status          TEXT    NOT NULL DEFAULT 'uploading'
                  CHECK (status IN ('uploading','ready')),
  uploaded_at     INTEGER NOT NULL,
  is_deleted      INTEGER NOT NULL DEFAULT 0,
  deleted_at      INTEGER                    -- purged from B2 30 days after this
);

CREATE INDEX idx_files_gallery ON files(user_id, is_deleted, status, taken_at DESC);
CREATE INDEX idx_files_folder  ON files(user_id, folder_id, is_deleted);
CREATE INDEX idx_files_sha1    ON files(user_id, sha1);
CREATE INDEX idx_files_purge   ON files(is_deleted, deleted_at) WHERE is_deleted = 1;
CREATE INDEX idx_files_orphan  ON files(status, uploaded_at) WHERE status = 'uploading';
CREATE INDEX idx_files_thumbjob ON files(thumb_status, thumb_attempts) WHERE thumb_status = 'unsupported';

CREATE TABLE shares (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  file_id         INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  token           TEXT    NOT NULL UNIQUE,   -- 32 bytes crypto/rand, base64url (256-bit)
  expires_at      INTEGER NOT NULL,
  revoked_at      INTEGER,
  view_count      INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_shares_token ON shares(token);

-- Cached B2 credentials, so a restart does not re-spend Class C transactions.
CREATE TABLE b2_tokens (
  scope           TEXT PRIMARY KEY,          -- e.g. 'download:users/1/'
  token           TEXT NOT NULL,
  expires_at      INTEGER NOT NULL
);
```

### Notes on the schema

- **No `b2_url` or `thumbnail_url` columns.** This was the central flaw in the original design: a private bucket means a stored URL is either useless or a permanent leak. URLs are composed at request time from `object_key` plus the cached prefix token.
- **`storage_used` is only ever mutated inside the same transaction** that inserts or soft-deletes a `files` row. Any other pattern drifts. A weekly reconciliation job asserts it against `SUM(file_size)` and logs a warning on mismatch.
- **Timestamps are unix integers**, not `TIMESTAMP`. SQLite has no real date type, and mixing formats across the two earlier documents would have produced comparison bugs.
- **`taken_at` drives gallery ordering**, not `uploaded_at`. A photo library sorted by upload time is useless after the first bulk import.

### B2 object layout

```
users/{user_id}/orig/{object_key}.{ext}
users/{user_id}/thumb/{object_key}.webp
users/{user_id}/meta/{object_key}.json       <- disaster recovery, see §7.3
users/{user_id}/db/cloud-YYYYMMDD.sqlite     <- nightly backup, see §7.3
```

---

## 4. API

All responses are JSON. Errors are `{"error": {"code": "...", "message": "..."}}` with meaningful HTTP status codes. Authentication is an httpOnly cookie; there is no CORS on this API because the SPA is served from the same origin.

### 4.1 Auth

```
POST   /api/auth/login      {username, password}  -> sets httpOnly cookie
POST   /api/auth/logout                           -> sets sessions.revoked_at
GET    /api/auth/me                               -> {username, storage_used, storage_quota}
```

Cookie: `sid=<32 random bytes, base64url>; HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=2592000`.

Only `sha256(token)` is stored. Every request looks up the session, checks `expires_at` and `revoked_at`, and slides the expiry when it is more than a day old.

There is no `/register` and no `/refresh`. Users are created with:

```
./cloudapp createuser --username faidz
```

### 4.2 Upload — three-phase, because the bytes bypass the server

```
POST   /api/files/init
       {filename, size, mime_type, folder_id?, sha1?}
    -> 200 {
         file_id, object_key,
         orig:  { b2_file_name, upload_url, upload_token, part_size?, large_file_id? },
         thumb: { b2_file_name, upload_url, upload_token }
       }
    -> 409 {code: "duplicate", file_id}   when sha1 matches an existing ready file
    -> 413                                when size would exceed storage_quota
```

The server validates MIME type against an allowlist, checks the quota, inserts the `files` row with `status='uploading'`, and mints B2 upload credentials. For `size > 100 MB` it calls `b2_start_large_file` and returns `large_file_id` plus `part_size`.

```
POST   /api/files/init-parts   {file_id, count}
    -> {urls: [{upload_url, upload_token}, ...]}
```

Each B2 upload URL serves **one upload at a time**. Parallel part uploads require one URL per concurrent worker. Request four.

```
POST   /api/files/:id/complete
       {b2_file_id, sha1, width?, height?, duration_sec?, taken_at?, thumb_ok, part_sha1s?}
    -> {file}
```

The server:

1. calls `b2_finish_large_file` when the upload was multipart;
2. calls `b2_get_file_info` and **verifies the reported size and SHA1 against B2's own record** — a client that lies about size would otherwise walk straight through the quota;
3. in one transaction: sets `status='ready'`, writes the dimensions, and increments `storage_used`;
4. writes the recovery sidecar to `users/{id}/meta/{object_key}.json`.

Steps 2 and 3 are what make trusting the browser with direct upload safe.

```
POST   /api/files/:id/abort?large_file_id=...
```

A client that gives up says so, and the server cancels the multipart upload and
drops the reserved row immediately. Without it the orphan sweep finds the mess a
day later, and B2 bills the uploaded parts of an unfinished large file for that
whole day.

Note what the server can and cannot check. It never sees a byte, so it cannot
verify content type at all — the sniffed `ftyp` brand that actually decides is
read in the browser. What it does enforce is the thing that costs money when it
is wrong: the byte count, checked against B2 itself.

### 4.3 Files

```
GET    /api/files?folder_id=&cursor=&limit=50&kind=&q=
    -> {items: [...], next_cursor}
```

Keyset pagination on `(taken_at, id)`, not `OFFSET` — offset pagination degrades on exactly the workload a photo library has.

Every item carries ready-to-use URLs composed server-side from the cached prefix token:

```json
{
  "id": 42, "filename": "IMG_0042.HEIC", "kind": "image",
  "width": 4032, "height": 3024, "taken_at": 1757462400,
  "thumb_url": "https://cdn.faidz.fun/f/users/1/thumb/a3f...c1.webp?Authorization=...",
  "orig_url":  "https://cdn.faidz.fun/f/users/1/orig/a3f...c1.heic?Authorization=..."
}
```

```
GET    /api/files/:id
PATCH  /api/files/:id            {filename?, folder_id?}
DELETE /api/files/:id            soft delete; decrements storage_used in the same tx
POST   /api/files/bulk-delete    {ids: [...]}
POST   /api/files/:id/restore    back out of the trash; re-charges storage_used
GET    /api/files?deleted=1      the trash view
```

A file belonging to someone else returns 404, never 403 — a 403 would confirm
the id exists.

`?q=` searches `filename` with `LIKE '%...%'`. At this scale that is correct, and an FTS5 table would be premature.

### 4.4 Folders

```
GET    /api/folders
POST   /api/folders              {name}
PATCH  /api/folders/:id          {name}
DELETE /api/folders/:id          -> 409 unless ?move_to=root or ?cascade=1
```

Deleting a non-empty folder is refused by default. `cascade=1` soft-deletes the contained files, which keeps them recoverable for 30 days.

### 4.5 Shares — these deliberately do NOT go direct to B2

```
POST   /api/shares               {file_id, expires_in_days=7}  -> {url}
DELETE /api/shares/:id
GET    /api/shares
GET    /s/:token                 public, no auth -> 302 redirect
```

A share link resolves through the VPS, which issues a **fresh 10-minute** B2 download authorization scoped to that single object and returns a 302. The bytes still come straight from B2 — the VPS forwards a redirect, not a file.

This is the one place where a direct long-lived B2 URL would be wrong: a recipient can save a signed URL, and revoking the share would not stop them until the token expired. The redirect indirection makes `revoked_at` take effect immediately, at a cost of one cheap request.

`/s/:token` is rate-limited to 60 requests per minute per IP. Tokens are 256-bit, so enumeration is not a concern, but the limit caps abuse of a leaked link.

### 4.6 Ops

```
GET    /api/health   -> {status, version, db_ok, b2_ok, storage_used, uptime_sec}
```

---

## 5. CLIENT-SIDE MEDIA PIPELINE

This replaces the server-side thumbnail generation from the earlier plan. It is the direct consequence of D1/D2 and the part most likely to surprise during implementation, so it is specified in detail.

### 5.1 Images

```js
const bitmap = await createImageBitmap(file, { imageOrientation: 'from-image' });
```

`imageOrientation: 'from-image'` applies the EXIF orientation flag. Without it, every portrait photo taken on a phone renders sideways — a bug that was not accounted for anywhere in the previous documents.

Scale so the long edge is 400px, draw to an `OffscreenCanvas`, then `convertToBlob({ type: 'image/webp', quality: 0.82 })`. Typical output is 15–30 KB.

**HEIC.** iPhone photos are HEIC by default, and this is a photo cloud, so this path is load-bearing:

- Safari and iOS decode HEIC natively through `createImageBitmap`. This covers the primary use case.
- Chrome and Firefox on desktop do not. Detection is a `try`/`catch` around `createImageBitmap`.
- Fallback: lazy-load `heic2any` (WASM, ~1.2 MB, fetched only on failure), decode, then continue.
- If that also fails, upload the original anyway and set `thumb_status='unsupported'`, which hands the file to the Tier 3 worker in §5.4. **Never block an upload on thumbnail failure** — the original is the thing that matters.

ProRAW `.DNG` takes the Tier 2b path instead: the embedded JPEG preview is sliced out of the TIFF structure and fed to `createImageBitmap` as a normal JPEG. See §5.4.

EXIF `DateTimeOriginal` is parsed in the browser from the first 64 KB of the file and sent as `taken_at`.

### 5.2 Videos

Load into a detached `<video>` element with `preload="metadata"`, seek to `min(1.0, duration * 0.1)`, wait for `seeked`, draw to canvas, encode to WebP.

This yields `width`, `height`, and `duration_sec` at the same time, at no extra cost.

HEVC/H.265 recordings from iPhone play in Safari but not in Chrome on desktop, and iPhone video arrives in a QuickTime `.MOV` container rather than `.MP4`. When the decode fails, the upload proceeds and the row is marked `thumb_status='unsupported'` for the Tier 3 worker in §5.4 to pick up — there is no browser-side fallback for HEVC.

### 5.3 Upload transport

| File size | Method |
|---|---|
| ≤ 100 MB | `b2_upload_file`, single request. SHA1 via `crypto.subtle.digest('SHA-1', ...)` on the whole blob. |
| > 100 MB | Large file API. **10 MB parts**, 4 concurrent. SHA1 computed per part, so peak memory is ~40 MB regardless of file size. |

10 MB parts keep a 500 MB file at 50 parts, far under B2's 10,000-part ceiling, and keep browser memory flat. This is also the only way the original plan's "10 MB/s multi-threaded" target is reachable — a single-stream upload will not get there.

Failed parts retry three times with exponential backoff. A failed part costs 10 MB of re-upload, not the whole file — which is the entire reason for using the large file API at a 500 MB cap.

Concurrency across files is capped at 2 to avoid saturating an upstream link.

### 5.4 Format coverage

The goal is that **every uploaded file ends up with a real thumbnail**, not a generic icon. That target, not architectural purity, is what decides where each format is handled.

#### What an iPhone actually produces

| Source | Container | Codec | Typical size |
|---|---|---|---|
| Photo (High Efficiency) | `.HEIC`, `ftyp` brand `heic`/`heix`/`mif1` | HEVC intra | 1–3 MB |
| Photo (Most Compatible) | `.JPG` | JPEG | 2–5 MB |
| Video (High Efficiency) | **`.MOV`**, `ftyp` brand `qt  ` | HEVC | ~170 MB per 4K minute |
| Video (Most Compatible) | `.MOV` | H.264 | ~350 MB per 4K minute |
| Live Photo | `.HEIC` plus a 3-second `.MOV` | both | a pair |
| Slo-mo | `.MOV` | HEVC, 120/240 fps | large |
| ProRAW | `.DNG` | Adobe DNG | 25–75 MB |
| ProRes | `.MOV` | ProRes 422 | ~6 GB per minute |

Note the container: iPhone video is **QuickTime `.MOV`**, not `.MP4`. The MIME allowlist in §8 must include `video/quicktime`, and `canPlayType('video/quicktime')` returns `""` in Chrome even though Chrome plays H.264 inside MOV perfectly well — so a codec support table is advisory and the per-file decode attempt is authoritative.

#### Two facts that shape the whole design

1. **Every browser on iOS is WebKit.** Chrome and Firefox on iOS are WKWebView, so an upload from the phone always gets Safari's decoders: HEIC natively, HEVC in hardware. The primary path has full coverage with no fallback at all.

2. **iOS usually transcodes HEIC to JPEG at the file picker.** Selecting from the Photo Library through `<input type="file">` hands the page a converted JPEG, not the HEIC, and the filename degrades to `image.jpg`. Picking through the Files app yields the true HEIC bytes.

   This is not fought. Accepting the transcode means the decode always succeeds, EXIF survives, and the WASM fallback is never fetched, at a cost of roughly 2× storage — about $0.30 a month more at 50 GB. Worth it. The `accept` attribute still lists `.heic,.heif` so a Files-app upload keeps its original, and the sniffed `ftyp` brand records which actually arrived.

#### The tiers

| Tier | Mechanism | Runs on | Covers |
|---|---|---|---|
| 1 | `createImageBitmap`, or `<video>` seek plus canvas | browser | JPEG, PNG, WebP, GIF; HEIC on Apple; H.264 anywhere; HEVC on Apple and on Windows with the HEVC extension |
| 2a | `heic2any` (libheif WASM, ~1.2 MB, lazy) | browser | HEIC on Chrome and Firefox desktop |
| 2b | **Embedded JPEG preview extraction** | browser | ProRAW `.DNG` |
| 3 | `ffmpeg` fallback worker | VPS | HEVC `.MOV` on non-Apple desktop, ProRes, and anything else that reached this point |

**Tier 2b** is worth calling out because it costs nothing. A DNG is a TIFF, and Apple ProRAW embeds a full-size JPEG preview inside it. Walking the IFD and SubIFD chain for an entry whose `Compression` is 7, then slicing those bytes straight out of the `File`, produces a normal JPEG that `createImageBitmap` handles. No raw decoding, no library, roughly 80 lines. ProRAW would otherwise be a guaranteed Tier 3.

There is deliberately **no Tier 2 for HEVC video**. `ffmpeg.wasm` exists but is ~25 MB and decoding HEVC from a 200 MB file inside a browser tab is not viable. That gap is exactly what Tier 3 is for.

#### Tier 3: the fallback worker

Claimed directly from the `files` table, **one at a time**, for rows where `thumb_status='unsupported'` and `thumb_attempts < 3`. No separate queue table and no in-memory state, so a restart resumes exactly where it stopped:

```
ffmpeg -ss <10% of duration> -i "<signed B2 URL>" -frames:v 1 -vf scale=400:-1 -f webp -
```

The cost of this is much lower than it looks. ffmpeg's HTTP protocol issues byte-range requests, and B2 honours them. For an iPhone `.MOV`, whose `moov` atom sits at the end of the file, ffmpeg reads the tail to get the sample table, then seeks directly to the target keyframe. A 200 MB video costs roughly **5–20 MB of transfer and about a second of CPU**, not a 200 MB download.

Guards: `nice -n 19`, a 60-second timeout, `MemoryMax` from the systemd unit, one concurrent job, three attempts before the row is marked `failed` for good. Egress from B2 to the VPS counts against the free allowance of 3× stored bytes per month, which a rare-path worker will not come close to exhausting.

Expected hit rate is near zero for uploads from the phone. It matters for a desktop bulk import of HEVC files, where it turns a wall of grey icons into a real gallery.

#### Playback is a separate problem from thumbnails

Tier 3 gives a Chrome-desktop user a poster frame for an HEVC clip, but Chrome still cannot *play* it. Server-side transcoding to H.264 remains a non-goal: it costs minutes of CPU per video and would dominate the VPS.

Instead the viewer detects this and says so. When `canPlayType` reports no support for the file's codec, the viewer shows the poster frame with a Download control and a line explaining the clip plays natively on the device that recorded it. On the owner's own iPhone and on Safari, playback is native and this state never appears.

#### Thumbnail encoding itself

Canvas WebP encoding requires **Safari 16.4 or newer**. Below that, `toBlob('image/webp')` silently returns a PNG — roughly 4× larger, with no error raised. The client reads `blob.type` back and records the real format, so `has_thumb` never implies a format that was not produced.

#### Resulting statuses

`thumb_status` takes one more value than the earlier draft allowed:

- `ready` — a thumbnail exists, from any tier
- `pending` — uploaded, Tier 3 job queued
- `failed` — Tier 3 attempted and gave up; generic icon, original intact
- `unsupported` — client could not decode and Tier 3 is not applicable

---

## 6. FRONTEND

Four screens. No router library beyond `react-router-dom`.

**`/login`** — username, password, error state.

**`/`** — gallery. Virtualized grid (`@tanstack/react-virtual`), thumbnails `loading="lazy"`, sorted by `taken_at` descending, infinite scroll on the keyset cursor. Left sidebar: folder list and a storage usage bar. Top bar: search, kind filter, upload button. Whole-window drag-and-drop.

**File viewer** — a modal, not a route change, so closing it does not reload the grid. Left/right arrow navigation, `<img>` or `<video controls>` pointed at `orig_url`, metadata panel, and download / share / delete actions.

**`/settings`** — change password, storage breakdown by kind, active sessions with a revoke button, trash view with restore.

Upload progress is a persistent bottom-right panel showing per-file progress and surviving navigation.

Auth state comes from `GET /api/auth/me`; a 401 from any request redirects to login. No token is ever read or written by JavaScript — the cookie is httpOnly, which removes token theft via XSS as a class of attack.

---

## 7. OPERATIONS

### 7.1 SQLite correctness

Non-negotiable, because the default configuration will produce `database is locked` under concurrent uploads:

Two pools against the same file. Writes go through one pinned connection so they serialize in Go instead of colliding inside SQLite; reads go through a second pool that WAL lets run concurrently alongside a write in flight.

```go
// writer
w.SetMaxOpenConns(1)
w.SetMaxIdleConns(1)
w.SetConnMaxLifetime(0)
// DSN: file:cloud.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)
//                   &_pragma=synchronous(1)&_txlock=immediate

// reader
r.SetMaxOpenConns(4)
```

`_txlock=immediate` matters more than it looks: it takes the write lock when the transaction begins rather than when it first writes, which removes the deferred-to-exclusive upgrade that is the usual source of `SQLITE_BUSY`. `journal_mode` is a property of the file rather than a connection, so it is set once at open and the result verified — a database that silently stayed in `delete` mode would block every reader on every write.

The earlier plan's "SQLite connection pooling" checklist item, taken literally without any of this, is the cause of the lock errors rather than the cure.

The driver is `modernc.org/sqlite`, not `mattn/go-sqlite3`. The latter needs cgo, which breaks decision D6's single static binary and makes cross-compiling from a Windows dev machine to the Linux VPS painful. The pure-Go driver is somewhat slower, which is irrelevant for a metadata-only workload, and `GOOS=linux go build` just works.

### 7.2 Background jobs

A single goroutine ticking once per hour, with every job idempotent and resumable from database state — no in-memory queue, so a restart loses nothing.

| Job | Schedule | Action |
|---|---|---|
| Orphan sweep | hourly | `status='uploading'` older than 24h: delete from B2 if present, delete the row. |
| Thumbnail fallback | every 2 min | One `thumb_status='unsupported'` row with `thumb_attempts < 3`: run the §5.4 ffmpeg extraction, upload the result, set `thumb_status='ready'` and `thumb_tier='ffmpeg'`. One job at a time. |
| Purge | daily 03:00 | `is_deleted=1 AND deleted_at < now-30d`: `b2_delete_file_version` on original, thumbnail, and sidecar, then hard-delete the row. **This is what stops paying B2 for deleted files.** |
| Token refresh | hourly | Renew the prefix download authorization at 6 days of age. |
| DB backup | daily 03:30 | `VACUUM INTO` a temp file, upload to `users/1/db/`, keep 14 days. |
| Reconcile | weekly | Compare `storage_used` against `SUM(file_size)`; log a warning on drift. |

### 7.3 Disaster recovery

The database is the only irreplaceable component — B2 holds the bytes, but without SQLite there are no filenames, no folders, and no dates.

Two independent layers of protection:

1. **Nightly `VACUUM INTO` plus upload to B2.** Restore is a download and a file copy.
2. **Per-file JSON sidecars** at `users/{id}/meta/{object_key}.json`, written at upload completion. If both the VPS and every database backup are lost, `b2_list_file_names` over the `meta/` prefix rebuilds the entire index. The earlier design created this path but never used it; it is now the second line of defence.

`./cloudapp reindex --from-b2` implements the rebuild. Write it on day one, while the sidecar format is fresh — an untested recovery procedure is not a recovery procedure.

### 7.4 Nginx

```nginx
server {
    listen 443 ssl http2;
    server_name cloud.faidz.fun;

    client_max_body_size 1m;   # API is JSON only — bytes go straight to B2

    location /api/ { proxy_pass http://127.0.0.1:8080; }
    location /s/   { proxy_pass http://127.0.0.1:8080; }
    location / {
        root /var/www/cloud;
        try_files $uri /index.html;   # SPA fallback
    }
}
```

Note that `client_max_body_size` stays at the 1 MB default and this is correct — a consequence of D1. Under the original architecture it would have had to be raised to 500 MB, and the omission would have caused an immediate `413` on the first real upload.

### 7.5 Deployment

`systemd` unit with `Restart=always`, `MemoryMax=384M`, `NoNewPrivileges=true`, and `ProtectSystem=strict` with a `ReadWritePaths` for the database directory. Environment from `/etc/cloudapp/env`, mode `0600`.

`ffmpeg` is the only external binary, required solely by the §5.4 fallback worker. Its absence must degrade rather than crash: the worker logs once and leaves those rows at `unsupported`. `MemoryMax` is 384M rather than 256M to leave headroom for a single HEVC keyframe decode.

Frontend is `npm run build` output rsynced to `/var/www/cloud`.

---

## 8. SECURITY

| Control | Implementation |
|---|---|
| Password storage | bcrypt cost 12 |
| Session tokens | 256-bit random; only the SHA-256 hash is stored; revocable |
| Cookie | httpOnly, Secure, SameSite=Lax |
| CSRF | SameSite=Lax plus an `Origin` header check on all mutating requests |
| Registration | disabled entirely (D7) |
| Data isolation | every query filters on `user_id`; verified by test |
| SQL injection | parameterized queries only; no string concatenation anywhere |
| Upload credential scope | write-only, prefix-restricted, short-lived (§2.3) |
| Quota bypass | prevented by server-side `b2_get_file_info` verification (§4.2) |
| Share revocation | effective immediately via the redirect indirection (§4.5) |
| Rate limits | login 5/min/IP, `/s/:token` 60/min/IP, `/api/files/init` 120/min/user |
| MIME allowlist | `image/jpeg,png,webp,gif,heic,heif,x-adobe-dng` and `video/mp4,quicktime,webm`. Sniffed `ftyp` brand decides, not the client-declared type — iOS reports an empty `file.type` often enough that trusting it rejects real photos. |
| Logging | never logs tokens, cookies, passwords, or B2 credentials |
| Secrets | env file mode 0600; no credentials in the repository |

### Residual risk, accepted

The browser holds a B2 upload token for the duration of an upload. Scoped to `writeFiles` under `users/1/` only, it cannot read, list, or delete. Worst case for a leaked token is junk written under that prefix until it expires, cleaned up by the orphan sweep. This is the price of D1 and it is a reasonable one.

---

## 9. COST

Honest figures, replacing the "$0.40/month" estimate.

| Item | 50 GB | 200 GB | 1 TB |
|---|---|---|---|
| B2 storage @ $6/TB/mo | $0.30 | $1.20 | $6.00 |
| Egress via Cloudflare | $0.00 | $0.00 | $0.00 |
| Class A transactions | $0.00 | $0.00 | $0.00 |
| Class B/C (under 2,500/day) | $0.00 | $0.00 | $0.00 |
| Cloudflare free plan | $0.00 | $0.00 | $0.00 |
| **Total beyond the existing VPS** | **$0.30** | **$1.20** | **$6.00** |

Storage cost is linear and unbounded — this is why D9 exists. Set `storage_quota` to a deliberate number and configure a Backblaze budget alert (Caps & Alerts) at roughly twice the expected spend. The first 10 GB of B2 storage are free.

Without Cloudflare in front, egress beyond the free allowance (3× stored bytes per month) costs $0.01/GB, which is what would actually have shown up on the bill under the original plan.

---

## 10. PERFORMANCE TARGETS

Reconciled — the previous documents specified both `<2s` and `<300ms` for page load, and a meaningless "100+ concurrent users" for a single-user application.

| Metric | Target | How it is achieved |
|---|---|---|
| Gallery first paint | < 800 ms | Small JSON payload, CDN-cached thumbnails |
| Thumbnail load (warm) | < 50 ms | Cloudflare edge cache |
| Upload throughput | ≥ 10 MB/s | 4 parallel 10 MB parts, direct to B2 |
| Video start / seek | < 1 s | Native `<video>` with Range requests against B2 |
| Gallery query, 100k rows | < 20 ms | Keyset pagination on a covering index |
| VPS memory | < 40 MB RSS | No file bytes, no image decoding on the server |
| VPS CPU during upload | ~0% | Bytes never touch the VPS |

---

## 11. BUILD ORDER

Ordered so that the highest-risk work is proven first and each step is independently verifiable.

**Step 0 — de-risk (do this before anything else).**
A throwaway script that, end to end: authorizes against B2, uploads a 200 MB file via the large file API, requests a prefix download authorization, and downloads it back through Cloudflare. Plus a single HTML page that picks a HEIC file from an iPhone and renders a thumbnail from it.

If Step 0 works, nothing else in this specification is technically uncertain. If HEIC decoding fails on the target device, that is discovered in the first hour rather than at the end.

**Step 1 — backend skeleton.** Go module, SQLite with migrations, config, `createuser` CLI, health endpoint, sessions and auth middleware.

**Step 2 — B2 service layer.** Account authorization with caching, upload URL minting, large file orchestration, prefix download authorization with caching, file info, delete.

**Step 3 — file API.** init / init-parts / complete, listing with keyset pagination, patch, soft delete, folders, quota accounting.

**Step 4 — frontend shell.** Vite, Tailwind, API client, login, app layout, TanStack Query wiring.

**Step 5 — upload pipeline.** The largest single piece of frontend work: SHA1, part splitting, concurrency control, retries, progress, thumbnail generation with the HEIC fallback chain.

**Step 6 — gallery and viewer.** Virtualized grid, modal viewer, folders, search, bulk selection.

**Step 7 — shares, settings, trash.**

**Step 8 — operations.** Background jobs, backup, `reindex --from-b2`, systemd unit, Nginx, deploy.

### On the one-day timeline

Steps 0–6 are a coherent, genuinely usable system: log in, upload photos and videos, browse them, view them, delete them. That is a realistic target for a focused day.

Steps 7 and 8 are the likely spillover. If time runs short, the correct things to cut are shares and the settings page. The correct things **not** to cut are the purge job (its absence means paying B2 forever for deleted files) and the database backup (its absence means one VPS failure destroys every filename and folder). Both are small — roughly 100 lines together — and both are cheap now and expensive to retrofit once real data exists.

---

## 12. DEFINITION OF DONE

- [ ] Upload a HEIC photo from an iPhone; a correctly oriented thumbnail appears in the grid
- [ ] Upload an HEVC `.MOV` from Chrome on desktop; the poster frame appears within a few minutes via Tier 3
- [ ] Upload a ProRAW `.DNG`; the embedded preview is extracted client-side, no server job queued
- [ ] Every file in the gallery has a real thumbnail; no generic icons remain
- [ ] Upload a 400 MB video; it resumes correctly after a deliberately failed part
- [ ] Video plays and seeks smoothly from the CDN
- [ ] `htop` shows no VPS CPU or memory spike during a large upload
- [ ] Gallery is responsive and usable on a phone
- [ ] Delete, then restore from trash, works
- [ ] Purge job actually removes the object from B2 (verified in the Backblaze console)
- [ ] Share link works, then stops working immediately after revocation
- [ ] `reindex --from-b2` reconstructs the database from an empty file
- [ ] `curl` against a second user's `file_id` returns 404, not the file
- [ ] Cloudflare reports `cf-cache-status: HIT` on repeat thumbnail loads
