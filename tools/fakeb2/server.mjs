// A local stand-in for Backblaze B2, enough of it to run the whole app.
//
// Not a test double for unit tests — those live in backend/internal/b2. This is
// a development fixture: it speaks the parts of the B2 API the browser and the
// Go server actually call, stores bytes on disk, and serves them back, so the
// full upload path works end to end without credentials or spend.
//
//   node tools/fakeb2/server.mjs --port 9000 --data ./.fakeb2
//
// Then point the backend at it:
//   B2_API_BASE=http://127.0.0.1:9000
//   B2_KEY_ID=any B2_APP_KEY=any B2_BUCKET_ID=bucket-1 B2_BUCKET_NAME=faidz-cloud
//   B2_UPLOAD_KEY_ID=upload B2_UPLOAD_APP_KEY=any
//   (leave B2_CDN_BASE unset so downloads resolve here too)

import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { dirname, join, resolve } from "node:path";

const args = process.argv.slice(2);
const flag = (name, fallback) => {
  const i = args.indexOf(`--${name}`);
  return i !== -1 && args[i + 1] ? args[i + 1] : fallback;
};

const PORT = Number(flag("port", "9000"));
const DATA = resolve(flag("data", ".fakeb2"));
const ORIGIN = flag("origin", "http://localhost:5173");
const BASE = `http://127.0.0.1:${PORT}`;

/** fileId -> { name, size, sha1, contentType, path } */
const files = new Map();
/** largeFileId -> { name, contentType, info, parts: Map<number, {path, sha1, size}> } */
const largeFiles = new Map();
let counter = 0;
const nextId = (prefix) => `${prefix}-${(++counter).toString(36)}-${Date.now().toString(36)}`;

await mkdir(DATA, { recursive: true });

// ---------------------------------------------------------------- helpers

function cors(res) {
  res.setHeader("Access-Control-Allow-Origin", ORIGIN);
  res.setHeader("Access-Control-Allow-Methods", "GET, POST, HEAD, OPTIONS");
  res.setHeader(
    "Access-Control-Allow-Headers",
    "authorization, content-type, x-bz-file-name, x-bz-content-sha1, x-bz-part-number, x-bz-info-large_file_sha1",
  );
  res.setHeader("Access-Control-Expose-Headers", "x-bz-file-id, x-bz-content-sha1");
  res.setHeader("Access-Control-Max-Age", "3600");
}

function json(res, status, body) {
  cors(res);
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}

function fail(res, status, code, message) {
  json(res, status, { status, code, message });
}

function readBody(req) {
  return new Promise((resolvePromise, reject) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => resolvePromise(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

async function store(id, buffer) {
  const path = join(DATA, `${id}.bin`);
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, buffer);
  return path;
}

const sha1 = (buffer) => createHash("sha1").update(buffer).digest("hex");

// ---------------------------------------------------------------- api

const api = {
  b2_authorize_account(_body, res, req) {
    const auth = req.headers.authorization ?? "";
    const decoded = Buffer.from(auth.replace(/^Basic\s+/i, ""), "base64").toString();
    const keyId = decoded.split(":")[0] ?? "";
    // Mirror the real key scoping so the server's health check and its refusal
    // to hand a master token to the browser both behave as they will in production.
    const allowed = keyId.startsWith("upload")
      ? { capabilities: ["writeFiles"], bucketId: "bucket-1", namePrefix: "users/1/" }
      : {
          capabilities: ["listFiles", "readFiles", "writeFiles", "deleteFiles", "shareFiles"],
          bucketId: "bucket-1",
          bucketName: "faidz-cloud",
          namePrefix: null,
        };
    return json(res, 200, {
      accountId: "fake-account",
      authorizationToken: `auth-${keyId}`,
      apiUrl: BASE,
      downloadUrl: BASE,
      recommendedPartSize: 100 * 1024 * 1024,
      absoluteMinimumPartSize: 5 * 1024 * 1024,
      allowed,
    });
  },

  b2_get_upload_url(_body, res) {
    json(res, 200, { uploadUrl: `${BASE}/upload`, authorizationToken: "upload-token" });
  },

  b2_start_large_file(body, res) {
    const id = nextId("large");
    largeFiles.set(id, {
      name: body.fileName,
      contentType: body.contentType,
      info: body.fileInfo ?? {},
      parts: new Map(),
    });
    json(res, 200, { fileId: id, fileName: body.fileName });
  },

  b2_get_upload_part_url(body, res) {
    if (!largeFiles.has(body.fileId)) return fail(res, 400, "bad_request", "no such large file");
    json(res, 200, {
      uploadUrl: `${BASE}/upload-part?fileId=${encodeURIComponent(body.fileId)}`,
      authorizationToken: "upload-token",
      fileId: body.fileId,
    });
  },

  async b2_finish_large_file(body, res) {
    const large = largeFiles.get(body.fileId);
    if (!large) return fail(res, 400, "bad_request", "no such large file");

    const expected = body.partSha1Array ?? [];
    const buffers = [];
    for (let i = 1; i <= expected.length; i++) {
      const part = large.parts.get(i);
      if (!part) return fail(res, 400, "bad_request", `part ${i} was never uploaded`);
      if (part.sha1 !== expected[i - 1]) {
        return fail(res, 400, "bad_request", `part ${i} checksum mismatch`);
      }
      buffers.push(await readFile(part.path));
    }

    const assembled = Buffer.concat(buffers);
    const id = nextId("file");
    const path = await store(id, assembled);
    files.set(id, {
      name: large.name,
      size: assembled.length,
      // B2 reports "none" for large files; the whole-file digest lives in
      // fileInfo. Reproducing that is the point of this fixture.
      sha1: "none",
      info: large.info,
      contentType: large.contentType,
      path,
    });
    for (const part of large.parts.values()) await rm(part.path, { force: true });
    largeFiles.delete(body.fileId);

    json(res, 200, {
      fileId: id,
      fileName: large.name,
      contentLength: assembled.length,
      contentSha1: "none",
      fileInfo: large.info,
    });
  },

  b2_cancel_large_file(body, res) {
    largeFiles.delete(body.fileId);
    json(res, 200, { fileId: body.fileId });
  },

  b2_get_file_info(body, res) {
    const file = files.get(body.fileId);
    if (!file) return fail(res, 404, "not_found", "no such file");
    json(res, 200, {
      fileId: body.fileId,
      fileName: file.name,
      contentLength: file.size,
      contentSha1: file.sha1,
      contentType: file.contentType,
      fileInfo: file.info ?? {},
    });
  },

  b2_get_download_authorization(body, res) {
    json(res, 200, {
      authorizationToken: `dl-${Buffer.from(body.fileNamePrefix ?? "").toString("base64url")}`,
    });
  },

  b2_list_file_names(body, res) {
    const prefix = body.prefix ?? "";
    // b2_list_file_names returns the CURRENT version of each name, not every
    // version — uploading the same name twice creates two versions in B2, and a
    // fixture that listed both would teach callers to expect duplicates that
    // the real API never shows them.
    const current = new Map();
    for (const [id, f] of files.entries()) {
      if (f.name.startsWith(prefix)) current.set(f.name, { fileId: id, fileName: f.name, contentLength: f.size });
    }
    const matching = [...current.values()].sort((a, b) => a.fileName.localeCompare(b.fileName));
    json(res, 200, { files: matching, nextFileName: null });
  },

  async b2_delete_file_version(body, res) {
    const file = files.get(body.fileId);
    if (!file) return fail(res, 400, "file_not_present", "already gone");
    await rm(file.path, { force: true });
    files.delete(body.fileId);
    json(res, 200, { fileId: body.fileId, fileName: file.name });
  },

  b2_list_buckets(_body, res) {
    json(res, 200, {
      buckets: [
        {
          bucketId: "bucket-1",
          bucketName: "faidz-cloud",
          bucketType: "allPrivate",
          corsRules: [{ corsRuleName: "browserUpload", allowedOrigins: [ORIGIN], allowedOperations: ["b2_upload_file", "b2_upload_part"] }],
          lifecycleRules: [{ fileNamePrefix: "", daysFromHidingToDeleting: 1 }],
        },
      ],
    });
  },
};

// ---------------------------------------------------------------- server

const server = createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", BASE);

  if (req.method === "OPTIONS") {
    cors(res);
    res.writeHead(204);
    return res.end();
  }

  try {
    // Browser-facing: the actual bytes.
    if (url.pathname === "/upload" && req.method === "POST") {
      const name = decodeURIComponent(req.headers["x-bz-file-name"] ?? "");
      const declared = req.headers["x-bz-content-sha1"];
      const buffer = await readBody(req);
      const digest = sha1(buffer);
      if (declared && declared !== "do_not_verify" && declared !== digest) {
        return fail(res, 400, "bad_request", "checksum did not match the uploaded bytes");
      }
      const id = nextId("file");
      const path = await store(id, buffer);
      files.set(id, {
        name,
        size: buffer.length,
        sha1: digest,
        contentType: req.headers["content-type"] ?? "application/octet-stream",
        path,
      });
      console.log(`  stored ${name} (${buffer.length} bytes)`);
      return json(res, 200, {
        fileId: id,
        fileName: name,
        contentLength: buffer.length,
        contentSha1: digest,
      });
    }

    if (url.pathname === "/upload-part" && req.method === "POST") {
      const fileId = url.searchParams.get("fileId") ?? "";
      const large = largeFiles.get(fileId);
      if (!large) return fail(res, 400, "bad_request", "no such large file");

      const partNumber = Number(req.headers["x-bz-part-number"]);
      const buffer = await readBody(req);
      const digest = sha1(buffer);
      if (req.headers["x-bz-content-sha1"] !== digest) {
        return fail(res, 400, "bad_request", "part checksum did not match");
      }
      const path = await store(`${fileId}-part-${partNumber}`, buffer);
      large.parts.set(partNumber, { path, sha1: digest, size: buffer.length });
      console.log(`  part ${partNumber} of ${large.name} (${buffer.length} bytes)`);
      return json(res, 200, { fileId, partNumber, contentLength: buffer.length, contentSha1: digest });
    }

    // Browser-facing: serving stored objects back, so thumbnails actually render.
    if (url.pathname.startsWith("/file/") && (req.method === "GET" || req.method === "HEAD")) {
      const wanted = decodeURIComponent(url.pathname.split("/").slice(3).join("/"));
      const entry = [...files.values()].find((f) => f.name === wanted);
      if (!entry) return fail(res, 404, "not_found", `no object named ${wanted}`);

      cors(res);
      const { size } = await stat(entry.path);
      const range = req.headers.range;
      if (range) {
        // Range support matters: it is how <video> seeks against B2.
        const match = /bytes=(\d*)-(\d*)/.exec(range);
        const start = match?.[1] ? Number(match[1]) : 0;
        const end = match?.[2] ? Number(match[2]) : size - 1;
        res.writeHead(206, {
          "Content-Type": entry.contentType,
          "Content-Range": `bytes ${start}-${end}/${size}`,
          "Content-Length": end - start + 1,
          "Accept-Ranges": "bytes",
        });
        if (req.method === "HEAD") return res.end();
        return createReadStream(entry.path, { start, end }).pipe(res);
      }

      res.writeHead(200, {
        "Content-Type": entry.contentType,
        "Content-Length": size,
        "Accept-Ranges": "bytes",
        "Cache-Control": "public, max-age=3600",
      });
      if (req.method === "HEAD") return res.end();
      return createReadStream(entry.path).pipe(res);
    }

    // Server-facing JSON API.
    if (url.pathname.startsWith("/b2api/v2/")) {
      const name = url.pathname.slice("/b2api/v2/".length);
      const handler = api[name];
      if (!handler) return fail(res, 404, "not_found", `fakeb2 does not implement ${name}`);
      const raw = await readBody(req);
      const body = raw.length > 0 ? JSON.parse(raw.toString()) : {};
      return await handler(body, res, req);
    }

    fail(res, 404, "not_found", `no route for ${url.pathname}`);
  } catch (error) {
    console.error("fakeb2 error", error);
    fail(res, 500, "internal", String(error));
  }
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`fakeb2 listening on ${BASE}`);
  console.log(`  data     ${DATA}`);
  console.log(`  cors     ${ORIGIN}`);
});
