# Development tools

| Tool | What it is for | Runs where |
|---|---|---|
| `mediaprobe/probe.html` | Step 0 probe: HEIC/HEVC decode, EXIF orientation, WebP thumbnails, SHA-1 throughput (§5.1–5.3) | The browser, especially the iPhone |
| `b2probe/` | Step 0 probe: large file upload, prefix download auth, CORS, key scoping (§2, §4.2, §5.3) | The dev machine, against a real bucket |
| `fakeb2/` | Local stand-in for B2, so the whole app runs without an account | The dev machine |

The two probes cover the only parts of `CLOUD_STORAGE_SPEC.md` that carried real
technical uncertainty. If both pass, nothing else in the spec is unproven.

---

## 1. Media probe

Published at the artifact URL handed over in chat. Open it **on the iPhone**, tap
**Choose photos & videos**, and pick straight from the camera roll — a portrait photo
(tests EXIF orientation), a Live Photo or burst shot, and a video over 100 MB.

Desktop browsers are a useful control but not the answer: Safari decodes HEIC natively
and Chrome does not, so a Chrome result says nothing about the real use case.

The page probes device capabilities on load and runs a synthetic self-test before any
file is chosen, so a blank result area means a script error, not a slow device.

### What each verdict means for the build

| Verdict | Consequence |
|---|---|
| Native decode, WebP thumbnails | Build §5.1 exactly as written. `heic2any` is never fetched. |
| heic2any fallback used on iPhone | Unexpected. Re-check the file really is HEIC (the card shows the `ftyp` brand). |
| Thumbnail is PNG, not WebP | Thumbnails run ~4× larger. Still works; storage and CDN figures in §9 rise. |
| Decode fails outright | `thumb_status='unsupported'`, generic icon in the grid. The original still uploads. |
| SHA-1 under ~40 MB/s | Hashing a 500 MB file gets slow. Consider `X-Bz-Content-Sha1: do_not_verify` on the single-file path. |

---

## 2. B2 probe

### Bucket setup

Install the Backblaze CLI, then:

```bash
b2 account authorize <MASTER_KEY_ID> <MASTER_APP_KEY>
b2 bucket create --default-server-side-encryption SSE-B2 faidz-cloud allPrivate
```

Apply the lifecycle rule so hidden versions stop being billed after a day (§2.1):

```bash
b2 bucket update --lifecycle-rule '{"fileNamePrefix":"","daysFromHidingToDeleting":1,"daysFromUploadingToHiding":null}' faidz-cloud allPrivate
```

Apply the CORS rules, without which direct browser upload fails (§2.2):

```bash
b2 bucket update --cors-rules '[{"corsRuleName":"browserUpload","allowedOrigins":["https://cloud.faidz.fun"],"allowedOperations":["b2_upload_file","b2_upload_part","b2_download_file_by_id","b2_download_file_by_name"],"allowedHeaders":["authorization","content-type","x-bz-file-name","x-bz-content-sha1","x-bz-part-number","x-bz-info-*"],"exposeHeaders":["x-bz-file-id","x-bz-content-sha1"],"maxAgeSeconds":3600}]' faidz-cloud allPrivate
```

Create the two application keys (§2.3). The master key is the one already in use; the
upload key is the restriction that makes handing a token to the browser safe:

```bash
b2 key create --bucket faidz-cloud --name-prefix 'users/1/' cloud-upload writeFiles
```

Both `keyID` and `applicationKey` are shown **once**. Save them immediately.

### Run

```bash
export B2_KEY_ID=...          # master key
export B2_APP_KEY=...
export B2_BUCKET_ID=...       # b2 bucket get faidz-cloud
export B2_BUCKET_NAME=faidz-cloud
export B2_UPLOAD_KEY_ID=...   # the writeFiles-only key
export B2_UPLOAD_APP_KEY=...
export B2_CDN_BASE=https://cdn.faidz.fun/f   # optional until Cloudflare is set up

cd tools/b2probe
go run . --size 20MB     # smoke test, single-file upload path
go run . --size 200MB    # the real one: large file API, 10 MB parts, 4 concurrent
```

`--size 200MB` crosses the 100 MB threshold and so exercises `b2_start_large_file`,
`b2_get_upload_part_url`, parallel `b2_upload_part`, and `b2_finish_large_file` — the
path the browser will take. `--size 20MB` stays on the single-request path.

Add `--keep` to leave the test objects in the bucket for inspection. Everything is
written under `users/1/` and deleted on the way out otherwise.

### Exit codes

`0` and `ALL CHECKS PASSED` means Step 1 can begin. Any `FAIL` line names the spec
section it violates. Two warnings are expected before Cloudflare exists:
`B2_CDN_BASE not set` and, if the restricted key has not been created yet,
the skipped blast-radius check.

---

## 3. Fake B2 (`fakeb2/`)

A development fixture, not a test double — the unit tests have their own in
`backend/internal/b2`. This speaks enough of the B2 API for the browser and the
Go server to run the whole upload path locally: it stores bytes on disk, serves
them back with CORS and Range support, and reproduces the awkward parts of the
real thing (large-file `contentSha1` reported as `"none"`, prefix-restricted
upload keys, per-part checksum rejection).

```bash
node tools/fakeb2/server.mjs --port 9000 --data ./.fakeb2
```

Then point the backend at it:

```bash
export B2_API_BASE=http://127.0.0.1:9000
export B2_KEY_ID=master B2_APP_KEY=secret
export B2_UPLOAD_KEY_ID=upload B2_UPLOAD_APP_KEY=secret
export B2_BUCKET_ID=bucket-1 B2_BUCKET_NAME=faidz-cloud
# leave B2_CDN_BASE unset so downloads resolve to the fixture too
```

Uploads, thumbnails, multipart, and the gallery all work against it with no
Backblaze account and no spend. It is not a substitute for `b2probe` against a
real bucket: only that can confirm CORS rules, lifecycle rules and key
restrictions behave as assumed.

---

## What became of the probes

`b2probe/main.go` is deliberately SDK-free, and its client, retry handling and
download-authorization caching became `backend/internal/b2` in Step 2. It also
caught a real bug on the way: `url.PathEscape` escapes `/` to `%2F`, which would
have flattened every nested object key.

The media probe was throwaway by design. Its logic was rewritten as
`frontend/src/upload/` in Step 5 — the sniffing, the EXIF reader, the DNG
preview extraction and the tiered decode chain all came from it — but its
measurements on a real iPhone are still what decide whether the fallback tiers
are ever exercised.
