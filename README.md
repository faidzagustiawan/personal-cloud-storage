# Personal Cloud Storage

Photos and videos for one person, on one small VPS, backed by Backblaze B2.

The defining decision is that **file bytes never pass through the server**. The
browser uploads straight to storage and downloads straight back; the VPS holds
metadata, hands out short-lived storage credentials, and verifies afterwards
that what landed matches what was declared. A 500 MB video costs the server a
few JSON round trips and nothing else.

Everything below is built and tested. What remains is configuration against a
real Backblaze account — see [HERMES.md](HERMES.md).

---

## What it does

- Upload photos and videos by drag-and-drop, including from an iPhone
- Thumbnails generated in the browser, with fallbacks for HEIC and a
  server-side ffmpeg worker for the formats no browser can decode
- Gallery with a virtualized grid, infinite scroll, search, folders and bulk
  selection
- Full-screen viewer with keyboard navigation
- Public share links that expire and can be revoked instantly
- Trash with 30-day recovery, then automatic purge from storage
- Nightly database backup to storage, plus per-file recovery sidecars

Deliberately not: team sharing, comments, AI search, face detection,
server-side transcoding, native mobile apps.

## Stack

| Layer | Choice | Why |
|---|---|---|
| Backend | Go, Gin, `modernc.org/sqlite` | One static binary; no cgo, so it cross-compiles to the VPS from anywhere |
| Database | SQLite in WAL mode | No second service to run or back up |
| Storage | Backblaze B2 native API | ~$6/TB/month, and free egress through Cloudflare |
| Frontend | React 18, Vite, TypeScript, Tailwind | 100 KB gzipped |
| Process | systemd | One binary; Docker would add 200 MB and buy nothing |

## Layout

```
CLOUD_STORAGE_SPEC.md    the specification — read this first
HERMES.md                what is left to do, and the prompt to continue with
backend/                 Go API, background jobs, CLI
frontend/                React SPA
tools/b2probe/           proves a real bucket behaves as assumed
tools/mediaprobe/        proves a real iPhone can decode its own photos
tools/fakeb2/            local stand-in for B2, so everything runs offline
deploy/                  systemd unit, Nginx config, deploy script
docs/historical/         superseded design notes, kept for context only
```

## Running it locally

No Backblaze account needed — `tools/fakeb2` stands in for storage.

```bash
# 1. storage
node tools/fakeb2/server.mjs --port 9000 --data ./.fakeb2

# 2. backend
cd backend
go build -o cloudapp .
CLOUD_DB=./cloud.db ./cloudapp createuser --username you
CLOUD_DB=./cloud.db \
CLOUD_ORIGINS=http://localhost:5173 \
B2_API_BASE=http://127.0.0.1:9000 \
B2_KEY_ID=master B2_APP_KEY=secret \
B2_UPLOAD_KEY_ID=upload B2_UPLOAD_APP_KEY=secret \
B2_BUCKET_ID=bucket-1 B2_BUCKET_NAME=faidz-cloud \
  ./cloudapp serve

# 3. frontend
npm run dev
```

Then open http://localhost:5173. Uploads, thumbnails, multipart, sharing and
the gallery all work against the fixture.

## Tests

```bash
cd backend && go test ./...     # 76 tests
npm run typecheck
npm run build
```

The tests cover the parts where a subtle bug costs real money or silently loses
photos: quota accounting, keyset pagination, the purge that stops B2 billing for
deleted files, share revocation, and cross-user isolation.

## Cost

| Stored | Per month |
|---|---|
| 50 GB | $0.30 |
| 200 GB | $1.20 |
| 1 TB | $6.00 |

Storage cost is linear and unbounded, which is why a hard per-user quota is
enforced and a budget alert belongs on the Backblaze account. Egress is free
through Cloudflare via the Bandwidth Alliance; without it, traffic past the free
allowance costs $0.01/GB.
