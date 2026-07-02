/**
 * Structured, pre-parsed stream attributes (SCOUT-03). Scout already parses the scene title to rank;
 * emitting the parse as a machine-readable object means the Den app renders quality/resolution/HDR/
 * source/etc. as badges WITHOUT re-parsing filenames on-device. Attached to each returned stream as a
 * custom `attributes` field (Stremio clients ignore unknown fields; Den decodes it).
 */
import { detectResolution, junkClass, type RawStream } from "./rank.js";
import { cleanLabel } from "./label.js";

export type StreamSource =
  | "remux"
  | "bluray"
  | "webdl"
  | "webrip"
  | "web"
  | "hdtv"
  | "dvdrip"
  | "cam"
  | "telesync"
  | "screener"
  | null;

export interface StreamAttributes {
  /** Coarse resolution bucket, or null when the title is untagged. */
  resolution: "2160p" | "1080p" | "720p" | "480p" | null;
  source: StreamSource;
  /** Video codec: avc (h264), hevc (h265/x265), av1. */
  codec: "avc" | "hevc" | "av1" | null;
  hdr: boolean;
  dolbyVision: boolean;
  /** Best audio format present, as a display label ("Atmos", "DTS:X", …), or null. */
  audio: string | null;
  threeD: boolean;
  sizeBytes: number | null;
  seeders: number | null;
  /** Debrid-cached (the reason this stream is in a cached-only list). */
  cached: boolean;
  /** The same human summary shown in `title` ("4K • WEB-DL • HDR • 18 GB"), for convenience. */
  label: string;
}

function detectSource(t: string): StreamSource {
  const junk = junkClass(t);
  if (junk === "cam" || junk === "telesync" || junk === "screener") return junk;
  if (/\bremux\b/.test(t)) return "remux";
  if (/bluray|blu-ray|b[dr][ .\-_]?rip/.test(t)) return "bluray";
  if (/web[ .\-_]?dl/.test(t)) return "webdl";
  if (/web[ .\-_]?rip/.test(t)) return "webrip";
  if (/\bweb\b/.test(t)) return "web";
  if (/\bhdtv\b/.test(t)) return "hdtv";
  if (/dvd[ .\-_]?rip/.test(t)) return "dvdrip";
  return null;
}

function detectCodec(t: string): "avc" | "hevc" | "av1" | null {
  if (/av1/.test(t)) return "av1";
  if (/x265|h\.?265|hevc/.test(t)) return "hevc";
  if (/x264|h\.?264|\bavc\b/.test(t)) return "avc";
  return null;
}

function detectAudio(t: string): string | null {
  if (/atmos/.test(t)) return "Atmos";
  if (/dts:x|dtsx|dts-x/.test(t)) return "DTS:X";
  if (/truehd|true-hd/.test(t)) return "TrueHD";
  if (/dts-hd|dts hd|dtshd/.test(t)) return "DTS-HD";
  if (/flac|lpcm|pcm/.test(t)) return "FLAC";
  if (/eac3|e-ac3|dd\+|ddp|ddplus/.test(t)) return "EAC3";
  if (/\bdts\b/.test(t)) return "DTS";
  if (/ac3|\bdd\b|dolby digital/.test(t)) return "AC3";
  return null;
}

export function streamAttributes(s: RawStream): StreamAttributes {
  const t = s.title.toLowerCase();
  // Scene titles tag Dolby Vision as "DV" as often as "DoVi"; DV is itself an HDR format.
  const dolbyVision = /dolby vision|dolbyvision|dovi|\bdv\b/.test(t);
  return {
    resolution: detectResolution(s.title),
    source: detectSource(t),
    codec: detectCodec(t),
    hdr: dolbyVision || /hdr10|\bhdr\b|\bhlg\b/.test(t),
    dolbyVision,
    audio: detectAudio(t),
    threeD: /\b3d\b|hsbs|half[ .\-_]?sbs|sbs[ .\-_]?3d/.test(t),
    sizeBytes: s.sizeBytes ?? null,
    seeders: s.seeders ?? null,
    cached: s.cached,
    label: cleanLabel(s),
  };
}
