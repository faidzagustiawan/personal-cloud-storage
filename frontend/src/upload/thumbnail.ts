import { parseJpegExif, parseTiffExif, type ExifData } from "./exif";
import type { Container } from "./sniff";

/**
 * The tiered thumbnail pipeline of spec §5.4.
 *
 * Tier 1  native decode — every iPhone upload, since all iOS browsers are WebKit
 * Tier 2a heic2any WASM — HEIC on Chrome and Firefox desktop
 * Tier 2b embedded JPEG preview — ProRAW .DNG, no raw processing needed
 * Tier 3  (server) ffmpeg — HEVC video outside Safari; not reachable from here
 *
 * A failure here is never fatal: the original still uploads and the row is
 * marked for the server-side worker. Losing a thumbnail is a cosmetic problem,
 * losing the photo is not.
 */

export const THUMB_MAX_EDGE = 400;

export type ThumbTier = "native" | "heic2any" | "dng-preview" | "video-frame";

export interface ThumbnailResult {
  blob: Blob;
  /** What the canvas actually produced. Safari below 16.4 silently gives PNG. */
  format: string;
  tier: ThumbTier;
  width: number;
  height: number;
  durationSec?: number;
}

export interface MediaProbe {
  thumbnail: ThumbnailResult | null;
  width?: number;
  height?: number;
  durationSec?: number;
  takenAt?: number;
  /** Why no thumbnail was produced, for the upload panel. */
  reason?: string;
}

export async function buildThumbnail(
  file: File,
  container: Container,
  kind: "image" | "video",
): Promise<MediaProbe> {
  try {
    if (kind === "video") return await probeVideo(file);
    return await probeImage(file, container);
  } catch (error) {
    return { thumbnail: null, reason: describe(error) };
  }
}

// ---------------------------------------------------------------- images

async function probeImage(file: File, container: Container): Promise<MediaProbe> {
  const exif = await readExif(file, container);
  let source: Blob = file;
  let tier: ThumbTier = "native";

  if (container.isRaw) {
    // Tier 2b. A DNG is a TIFF, and ProRAW embeds a full-size JPEG preview, so
    // the preview bytes can be sliced straight out and decoded as an ordinary
    // JPEG — no raw processing, no library.
    source = await extractDngPreview(file);
    tier = "dng-preview";
  }

  let bitmap: ImageBitmap;
  try {
    bitmap = await decode(source);
  } catch (nativeError) {
    if (!container.isHeic) throw nativeError;

    // Tier 2a. Lazily imported so the ~1.2 MB decoder is fetched only by the
    // browsers that actually need it, and from our own origin rather than a CDN.
    const { default: heic2any } = await import("heic2any");
    const converted = await heic2any({ blob: file, toType: "image/jpeg", quality: 0.92 });
    const jpeg = Array.isArray(converted) ? converted[0] : converted;
    if (!jpeg) throw nativeError;
    bitmap = await decode(jpeg);
    tier = "heic2any";
  }

  try {
    const thumbnail = await render(bitmap, bitmap.width, bitmap.height, tier);
    return {
      thumbnail,
      width: bitmap.width,
      height: bitmap.height,
      ...(exif?.takenAt !== undefined ? { takenAt: exif.takenAt } : {}),
    };
  } finally {
    bitmap.close?.();
  }
}

function decode(source: Blob): Promise<ImageBitmap> {
  // "from-image" applies the EXIF orientation flag. Without it every portrait
  // photo from a phone renders sideways.
  return createImageBitmap(source, { imageOrientation: "from-image" });
}

async function readExif(file: File, container: Container): Promise<ExifData | null> {
  const head = await file.slice(0, 65536).arrayBuffer();
  return container.isRaw ? parseTiffExif(head) : parseJpegExif(head);
}

// ---------------------------------------------------------------- DNG preview

/**
 * Walks the IFD and SubIFD chain for the largest JPEG-compressed image and
 * returns those bytes. Roughly eighty lines that turn a guaranteed tier 3 into
 * a tier 2.
 */
async function extractDngPreview(file: File): Promise<Blob> {
  const headSize = Math.min(file.size, 1 << 20);
  const view = new DataView(await file.slice(0, headSize).arrayBuffer());
  const littleEndian = view.getUint16(0) === 0x4949;
  const u16 = (o: number) => view.getUint16(o, littleEndian);
  const u32 = (o: number) => view.getUint32(o, littleEndian);
  if (u16(2) !== 42) throw new Error("not a TIFF header");

  const found: { offset: number; length: number }[] = [];
  const seen = new Set<number>();

  const walk = (dirOffset: number, depth: number): void => {
    if (depth > 3 || dirOffset <= 0 || dirOffset + 2 > view.byteLength) return;
    if (seen.has(dirOffset)) return;
    seen.add(dirOffset);

    const count = u16(dirOffset);
    if (count === 0 || count > 512 || dirOffset + 2 + count * 12 + 4 > view.byteLength) return;

    let compression = 0;
    let jpegOffset = 0;
    let jpegLength = 0;
    let stripOffset = 0;
    let stripLength = 0;

    for (let i = 0; i < count; i++) {
      const entry = dirOffset + 2 + i * 12;
      const tag = u16(entry);
      const type = u16(entry + 2);
      const valueCount = u32(entry + 4);
      const scalar = () => (type === 3 ? u16(entry + 8) : u32(entry + 8));

      if (tag === 0x0103) compression = scalar();
      else if (tag === 0x0201) jpegOffset = u32(entry + 8);
      else if (tag === 0x0202) jpegLength = u32(entry + 8);
      else if (tag === 0x0111 && valueCount === 1) stripOffset = scalar();
      else if (tag === 0x0117 && valueCount === 1) stripLength = scalar();
      else if (tag === 0x014a) {
        if (valueCount === 1) walk(u32(entry + 8), depth + 1);
        else {
          const at = u32(entry + 8);
          for (let k = 0; k < Math.min(valueCount, 16); k++) {
            if (at + k * 4 + 4 <= view.byteLength) walk(u32(at + k * 4), depth + 1);
          }
        }
      }
    }

    if (jpegOffset && jpegLength) found.push({ offset: jpegOffset, length: jpegLength });
    else if (compression === 7 && stripOffset && stripLength) {
      found.push({ offset: stripOffset, length: stripLength });
    }

    walk(u32(dirOffset + 2 + count * 12), depth); // next IFD in the chain
  };

  walk(u32(4), 0);
  if (found.length === 0) throw new Error("no embedded JPEG preview in this DNG");

  found.sort((a, b) => b.length - a.length);
  const best = found[0]!;
  if (best.offset + best.length > file.size) throw new Error("preview runs past end of file");
  return file.slice(best.offset, best.offset + best.length, "image/jpeg");
}

// ---------------------------------------------------------------- video

async function probeVideo(file: File): Promise<MediaProbe> {
  const url = URL.createObjectURL(file);
  const video = document.createElement("video");
  video.preload = "metadata";
  video.muted = true;
  video.playsInline = true;
  video.setAttribute("playsinline", "");
  video.src = url;

  try {
    await once(video, "loadedmetadata", 15_000);
    const { videoWidth: width, videoHeight: height, duration } = video;
    if (!width || !height) {
      // Chrome on desktop cannot decode HEVC, which is what an iPhone records
      // by default. The upload continues and the server picks it up (§5.4).
      throw new Error("this browser cannot decode the video track");
    }

    const target = Math.min(1, (Number.isFinite(duration) ? duration : 10) * 0.1);
    video.currentTime = target;
    await once(video, "seeked", 15_000);

    const thumbnail = await render(video, width, height, "video-frame");
    return {
      thumbnail: { ...thumbnail, durationSec: Number.isFinite(duration) ? duration : 0 },
      width,
      height,
      ...(Number.isFinite(duration) ? { durationSec: duration } : {}),
      takenAt: Math.floor(file.lastModified / 1000),
    };
  } catch (error) {
    return {
      thumbnail: null,
      takenAt: Math.floor(file.lastModified / 1000),
      reason: describe(error),
    };
  } finally {
    URL.revokeObjectURL(url);
    video.removeAttribute("src");
    video.load();
  }
}

function once(element: HTMLMediaElement, event: string, timeoutMs: number): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = window.setTimeout(() => {
      cleanup();
      reject(new Error(`${event} timed out`));
    }, timeoutMs);

    const onDone = () => {
      cleanup();
      resolve();
    };
    const onError = () => {
      cleanup();
      reject(new Error(`media error waiting for ${event}`));
    };
    function cleanup() {
      window.clearTimeout(timer);
      element.removeEventListener(event, onDone);
      element.removeEventListener("error", onError);
    }

    element.addEventListener(event, onDone, { once: true });
    element.addEventListener("error", onError, { once: true });
  });
}

// ---------------------------------------------------------------- rendering

async function render(
  source: CanvasImageSource,
  width: number,
  height: number,
  tier: ThumbTier,
): Promise<ThumbnailResult> {
  const scale = Math.min(1, THUMB_MAX_EDGE / Math.max(width, height));
  const w = Math.max(1, Math.round(width * scale));
  const h = Math.max(1, Math.round(height * scale));

  let blob: Blob | null;
  if (typeof OffscreenCanvas === "function") {
    const canvas = new OffscreenCanvas(w, h);
    canvas.getContext("2d")?.drawImage(source, 0, 0, w, h);
    blob = await canvas.convertToBlob({ type: "image/webp", quality: 0.82 });
  } else {
    const canvas = document.createElement("canvas");
    canvas.width = w;
    canvas.height = h;
    canvas.getContext("2d")?.drawImage(source, 0, 0, w, h);
    blob = await new Promise<Blob | null>((resolve) =>
      canvas.toBlob(resolve, "image/webp", 0.82),
    );
  }

  if (!blob) throw new Error("the canvas produced no image");

  // Safari below 16.4 has no WebP encoder and silently hands back a PNG. The
  // real type is recorded rather than assumed, so `thumb_format` never claims a
  // format that was not produced.
  return { blob, format: blob.type || "image/png", tier, width: w, height: h };
}

function describe(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
