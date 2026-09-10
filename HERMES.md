# Handover

The application is finished and tested. What is left is configuration against a
real Backblaze account and a real domain — work that could not be done without
credentials.

Everything so far was verified against `tools/fakeb2`, a local stand-in. That
proves the code is correct. It cannot prove that a real bucket is configured
the way the code assumes, which is what the first two steps below are for.

---

## The prompt

Paste this into a fresh session in the cloned repository.

> Read `CLOUD_STORAGE_SPEC.md` and `HERMES.md`, then work through the remaining
> steps in HERMES.md in order. Ask me for any credential you need rather than
> guessing. Do not skip step 1 — everything after it assumes the bucket behaves
> the way the spec says.

That is enough. The spec carries every decision already made, and each step
below says what it needs and how to tell it worked.

---

## What is left

### 1. Prove the real bucket — do this first

Nothing else is worth doing until this passes. It is the only thing that can
confirm CORS rules, lifecycle rules and key restrictions behave as assumed.

Create the bucket and both application keys following `tools/README.md` §2, then:

```bash
cd tools/b2probe
go run . --size 20MB     # single-file upload path
go run . --size 200MB    # multipart: 10 MB parts, 4 concurrent
```

**Needs:** a Backblaze B2 account.

**Done when:** it prints `ALL CHECKS PASSED`. Any `FAIL` line names the spec
section it violates.

Two warnings are expected at this stage: `B2_CDN_BASE not set`, and the skipped
blast-radius check if the restricted upload key does not exist yet. Create that
key — the server refuses to hand a browser anything else, and that refusal is
what makes direct-to-storage uploads safe.

### 2. Prove a real iPhone

Open `tools/mediaprobe/probe.html` **on the phone** and pick straight from the
camera roll: a portrait photo, a video over 100 MB, and a ProRAW file if the
phone shoots them.

**Needs:** the page served over HTTPS, or opened from a local file.

**Done when:** the verdict says the device can run the pipeline. What it reports
decides whether the fallback tiers are ever exercised at all — every browser on
iOS is WebKit, so the phone should decode everything natively.

Watch for one thing: whether the picker hands over a real `.HEIC` or a JPEG that
iOS converted on the way out. Both work. The JPEG is about twice the size, which
is worth roughly $0.30 a month at 50 GB.

### 3. Cloudflare

Without this, storage egress past the free allowance costs $0.01/GB. With it,
egress is free through the Bandwidth Alliance and thumbnails get cached.

Follow `CLOUD_STORAGE_SPEC.md` §2.5: a **proxied** CNAME from `cdn.faidz.fun` to
the B2 host, a transform rule rewriting `/f/*`, and a cache rule for thumbnails.

**Needs:** the domain on Cloudflare.

**Done when:** `curl -I` on a thumbnail URL returns a `cf-cache-status` header.
Its absence means the CNAME is not proxied — the orange cloud is the whole point.

### 4. Deploy

```bash
./deploy/deploy.sh user@your-vps
```

Read `deploy/README.md` first: the environment file with the storage credentials
is created by hand on the server and never shipped from a developer machine.

**Needs:** SSH access, a TLS certificate, and `ffmpeg` installed (optional —
without it the rare undecodable files keep a placeholder instead of getting a
server-side thumbnail).

**Done when:** `https://your-domain/api/health` reports `"status": "ok"` and
`"upload_key_scope"` naming the restricted prefix. If the upload key is
over-privileged, health says so rather than letting it pass.

### 5. Prove recovery, once

```bash
sudo -u cloudapp /opt/cloudapp/cloudapp reindex --from-b2 --username you --dry-run
```

**Done when:** it lists the files it would restore. This was tested end to end
against the fixture — database deleted entirely, rebuilt from the per-file
sidecars, originals still downloadable — but an untested recovery procedure on
the real bucket is not a recovery procedure.

### 6. Set a budget alert

Storage cost is linear and unbounded. Set a cap in the Backblaze console at
roughly twice expected spend, and set `CLOUD_DEFAULT_QUOTA` to a deliberate
number rather than leaving the 100 GB default unexamined.

---

## Known limits, already decided

These are not bugs and do not need solving.

- **No automatic camera-roll backup.** A web app cannot back up a phone's photos
  in the background. Uploads are manual. This is the single biggest functional
  gap versus Google Photos and it was accepted deliberately.
- **HEVC video does not play outside Safari.** The server extracts a poster
  frame so it still *displays*; playback would need transcoding, which would
  dominate a small VPS. The viewer says so and offers a download.
- **Folders are flat.** `parent_folder_id` exists in the schema and is always
  NULL, so nesting is a feature change rather than a migration.
- **Large files are not deduplicated.** A whole-file hash needs the whole file in
  memory, which is the thing multipart upload exists to avoid. Size is still
  verified against storage, which is the check that protects the quota.
- **Folders do not survive a reindex.** Sidecars do not record them, so a rebuild
  puts everything in the root.

## If something looks wrong

`/api/health` reports honestly rather than a flat `ok`. It distinguishes
unconfigured storage from broken storage, and names an over-privileged upload
key instead of letting it pass silently.

```bash
curl -s https://your-domain/api/health | jq
journalctl -u cloudapp -f
```

Background jobs run inside the server process, so they share its log. The purge
is the one that matters financially: a trashed row whose storage object is never
deleted is billed forever, invisibly.
