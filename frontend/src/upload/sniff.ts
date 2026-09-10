/**
 * Container detection from magic bytes.
 *
 * The declared `file.type` is not trustworthy here: iOS reports an empty string
 * often enough that trusting it rejects real photos, and the Files app and the
 * Photo Library disagree about HEIC. The `ftyp` brand is what actually decides
 * which decode path a file takes (spec §5.4).
 */

export interface Container {
  /** Human-readable brand, for the error surface. */
  brand: string;
  isHeic: boolean;
  isVideo: boolean;
  /** TIFF-based raw — ProRAW .DNG, which carries an embedded JPEG preview. */
  isRaw: boolean;
}

const HEIC_BRANDS = new Set(["heic", "heix", "hevc", "hevx", "mif1", "msf1", "heim", "heis"]);
const VIDEO_BRANDS = new Set(["isom", "mp42", "mp41", "iso2", "iso4", "iso5", "iso6", "qt", "M4V", "avc1"]);

export async function sniffContainer(file: File): Promise<Container> {
  const head = new Uint8Array(await file.slice(0, 32).arrayBuffer());
  const ascii = (offset: number, length: number) =>
    String.fromCharCode(...head.slice(offset, offset + length));

  const base: Container = { brand: "unrecognised", isHeic: false, isVideo: false, isRaw: false };

  if (head.length >= 12 && ascii(4, 4) === "ftyp") {
    const brand = ascii(8, 4).trim();
    return {
      brand: `ftyp/${brand}`,
      isHeic: HEIC_BRANDS.has(brand),
      // iPhone video is QuickTime (.MOV, brand "qt  "), not MP4.
      isVideo: VIDEO_BRANDS.has(brand),
      isRaw: false,
    };
  }

  if (head[0] === 0xff && head[1] === 0xd8) return { ...base, brand: "JPEG" };
  if (ascii(1, 3) === "PNG") return { ...base, brand: "PNG" };
  if (ascii(0, 4) === "RIFF" && ascii(8, 4) === "WEBP") return { ...base, brand: "WEBP" };
  if (ascii(0, 3) === "GIF") return { ...base, brand: ascii(0, 6) };

  const tiffLE = ascii(0, 2) === "II" && head[2] === 0x2a && head[3] === 0x00;
  const tiffBE = ascii(0, 2) === "MM" && head[2] === 0x00 && head[3] === 0x2a;
  if (tiffLE || tiffBE) return { ...base, brand: "TIFF/DNG", isRaw: true };

  return base;
}

/**
 * The MIME type sent to the server, which validates it against the allowlist in
 * spec §8. Derived from the sniffed container when the browser declined to
 * guess, which iOS regularly does.
 */
export function resolveMimeType(file: File, container: Container): string {
  if (file.type) return file.type.toLowerCase();

  if (container.isHeic) return "image/heic";
  if (container.isRaw) return "image/x-adobe-dng";
  if (container.isVideo) return "video/quicktime";

  const ext = file.name.split(".").pop()?.toLowerCase() ?? "";
  const byExtension: Record<string, string> = {
    jpg: "image/jpeg",
    jpeg: "image/jpeg",
    png: "image/png",
    gif: "image/gif",
    webp: "image/webp",
    heic: "image/heic",
    heif: "image/heif",
    dng: "image/x-adobe-dng",
    mp4: "video/mp4",
    mov: "video/quicktime",
    m4v: "video/mp4",
    webm: "video/webm",
  };
  return byExtension[ext] ?? "application/octet-stream";
}

export function kindOf(mimeType: string): "image" | "video" | null {
  if (mimeType.startsWith("image/")) return "image";
  if (mimeType.startsWith("video/")) return "video";
  return null;
}
