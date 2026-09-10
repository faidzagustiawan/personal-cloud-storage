/**
 * Minimal EXIF reader: orientation and DateTimeOriginal, nothing else.
 *
 * Orientation matters because it is the reason `createImageBitmap` is called
 * with `imageOrientation: "from-image"` — without it every portrait photo taken
 * on a phone lands sideways. DateTimeOriginal matters because the gallery is
 * ordered by when a photo was taken, not when it was uploaded; sorted by upload
 * time a library is useless after the first bulk import.
 */

export interface ExifData {
  orientation?: number;
  /** Unix seconds, parsed from "YYYY:MM:DD HH:MM:SS" in the camera's local time. */
  takenAt?: number;
}

/** JPEG: walk the segment chain to APP1, then read the TIFF block inside it. */
export function parseJpegExif(buffer: ArrayBuffer): ExifData | null {
  try {
    const view = new DataView(buffer);
    if (view.getUint16(0) !== 0xffd8) return null;

    let offset = 2;
    while (offset + 4 < view.byteLength) {
      if (view.getUint8(offset) !== 0xff) break;
      const marker = view.getUint8(offset + 1);
      if (marker === 0xda) break; // start of scan: no metadata past here
      const size = view.getUint16(offset + 2);
      if (marker === 0xe1 && view.getUint32(offset + 4) === 0x45786966) {
        return readTiff(view, offset + 10);
      }
      offset += 2 + size;
    }
  } catch {
    // Malformed EXIF is common and harmless. The upload proceeds either way.
  }
  return null;
}

/** DNG and other TIFF-based files: the IFD chain starts at byte zero. */
export function parseTiffExif(buffer: ArrayBuffer): ExifData | null {
  try {
    return readTiff(new DataView(buffer), 0);
  } catch {
    return null;
  }
}

export function readTiff(view: DataView, base: number): ExifData | null {
  const littleEndian = view.getUint16(base) === 0x4949;
  const u16 = (o: number) => view.getUint16(o, littleEndian);
  const u32 = (o: number) => view.getUint32(o, littleEndian);
  if (u16(base + 2) !== 42) return null;

  const out: ExifData = {};
  const seen = new Set<number>();

  const walk = (dirOffset: number, depth: number): void => {
    if (depth > 2 || dirOffset <= 0 || dirOffset + 2 > view.byteLength) return;
    if (seen.has(dirOffset)) return;
    seen.add(dirOffset);

    const count = u16(dirOffset);
    if (count === 0 || count > 512) return;

    for (let i = 0; i < count; i++) {
      const entry = dirOffset + 2 + i * 12;
      if (entry + 12 > view.byteLength) return;

      const tag = u16(entry);
      const type = u16(entry + 2);
      const valueCount = u32(entry + 4);

      if (tag === 0x0112) out.orientation = u16(entry + 8);
      else if (tag === 0x8769) walk(base + u32(entry + 8), depth + 1); // ExifIFD
      else if (tag === 0x9003 && type === 2 && valueCount > 1 && valueCount < 64) {
        const at = valueCount > 4 ? base + u32(entry + 8) : entry + 8;
        let text = "";
        for (let k = 0; k < valueCount - 1 && at + k < view.byteLength; k++) {
          text += String.fromCharCode(view.getUint8(at + k));
        }
        const parsed = parseExifDate(text);
        if (parsed) out.takenAt = parsed;
      }
    }
  };

  walk(base + u32(base + 4), 0);
  return out;
}

/**
 * EXIF timestamps are "YYYY:MM:DD HH:MM:SS" with no timezone, so they are read
 * as local time — which is what the camera meant.
 */
function parseExifDate(text: string): number | undefined {
  const match = /^(\d{4}):(\d{2}):(\d{2})[ T](\d{2}):(\d{2}):(\d{2})/.exec(text.trim());
  if (!match) return undefined;
  const [, y, mo, d, h, mi, s] = match;
  const date = new Date(
    Number(y),
    Number(mo) - 1,
    Number(d),
    Number(h),
    Number(mi),
    Number(s),
  );
  const seconds = Math.floor(date.getTime() / 1000);
  return Number.isFinite(seconds) && seconds > 0 ? seconds : undefined;
}
