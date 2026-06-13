#!/usr/bin/env bash
# Regenerate the committed M3 media e2e fixtures (tools/e2e/fixtures/media/).
#
# These fixtures are TINY (tens of KB), deterministic, and COMMITTED to the
# repo: CI runs tools/e2e/m3-media.sh against them and has NO `say` / Xcode
# Swift toolchain, so it never runs THIS script. Re-run it only on a macOS host
# (with `say`, `ffmpeg`, and the Xcode Swift toolchain) when you need to change
# the fixtures, then commit the regenerated files plus manifest.json.
#
# What it produces under tools/e2e/fixtures/media/:
#   speech.mp4    a few-second .mp4: a solid keyframed color video track muxed
#                 with `say`-synthesised speech of a KNOWN phrase (the ASR /
#                 spoken-phrase -> video@timestamp exit-criterion fixture).
#   blue.png      a clearly BLUE image with the word "OCEAN" drawn on it
#                 (CLIP text->image "blue" target + OCR word "OCEAN").
#   red.png       a clearly RED image with the word "SUNSET" drawn on it
#                 (the CLIP distractor + OCR word "SUNSET").
#   manifest.json the sidecar the test reads: the known phrase, the searchable
#                 rare words, the per-image color/word, and clip duration_ms.
#
# Determinism: `say` output varies slightly by macOS voice version, so the
# manifest records the ACTUAL measured duration of the regenerated clip; the
# test asserts the returned ASR timestamp falls within that duration rather
# than a hard-coded value.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT_DIR="${REPO_ROOT}/tools/e2e/fixtures/media"
TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# --- known content (kept in sync with manifest.json the test reads) ----------
# Phrase uses only single, whisper-verbatim words (no compounds like "roadmap"
# which whisper-tiny splits into "road map", breaking a one-word keyword match).
PHRASE="the quarterly planning review happens on tuesday afternoon"
# Words the test searches for; whisper-tiny must transcribe at least one
# verbatim for the spoken-phrase->video@timestamp arm to match.
ASR_WORDS='["tuesday","planning","afternoon"]'
# Leading silence (ms) prepended before the speech, so the transcript segment
# starts mid-clip and the deep-link timestamp is demonstrably non-zero.
LEAD_SILENCE_MS=2500
BLUE_WORD="OCEAN"
RED_WORD="SUNSET"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
need say
need ffmpeg
need ffprobe
need swift

mkdir -p "$OUT_DIR"

echo "== generating speech video (phrase: \"$PHRASE\") =="
say -o "$TMP/p.aiff" "$PHRASE"
DUR="$(ffprobe -v error -show_entries format=duration \
  -of default=noprint_wrappers=1:nokey=1 "$TMP/p.aiff")"
echo "   speech duration: ${DUR}s"

# Total clip = leading silence + speech, so the spoken phrase starts at
# ~LEAD_SILENCE_MS and its transcript segment has a non-zero start_ms.
TOTAL="$(python3 -c 'import sys;print(float(sys.argv[1])+float(sys.argv[2])/1000.0)' "$DUR" "$LEAD_SILENCE_MS")"
echo "   lead silence: ${LEAD_SILENCE_MS}ms; total clip: ${TOTAL}s"

# Tiny .mp4: 160x120 solid color at 8fps with a keyframe every 16 frames, muxed
# with the say audio DELAYED by LEAD_SILENCE_MS (adelay) and padded to the full
# duration (mono 16kHz AAC @ 24k). H.264 + faststart so any player /
# ffmpeg-based enrich worker can demux the audio track for whisper.
ffmpeg -v error -y \
  -f lavfi -i "color=c=0x1f4e79:s=160x120:d=${TOTAL}:r=8" \
  -i "$TMP/p.aiff" \
  -filter_complex "[1:a]adelay=${LEAD_SILENCE_MS}|${LEAD_SILENCE_MS},apad[a]" \
  -map 0:v -map "[a]" -shortest \
  -c:v libx264 -preset veryfast -crf 40 -pix_fmt yuv420p -g 16 \
  -c:a aac -b:a 24k -ac 1 -ar 16000 \
  -movflags +faststart \
  "$OUT_DIR/speech.mp4"

# Measure the muxed clip's real duration for the manifest (ms, integer).
CLIP_DUR="$(ffprobe -v error -show_entries format=duration \
  -of default=noprint_wrappers=1:nokey=1 "$OUT_DIR/speech.mp4")"
CLIP_MS="$(python3 -c 'import sys;print(int(round(float(sys.argv[1])*1000)))' "$CLIP_DUR")"
echo "   clip duration: ${CLIP_DUR}s (${CLIP_MS}ms)"

# --- color + text images (Swift / CoreText; ffmpeg here lacks drawtext) ------
# A self-contained CoreGraphics renderer: solid background + centred white word.
# No AppKit, so it works headless on a CI-less generator host.
cat >"$TMP/mkimg.swift" <<'SWIFT'
import Foundation
import CoreGraphics
import ImageIO
import CoreText
import UniformTypeIdentifiers

// args: <out.png> <width> <height> <r> <g> <b> <text>
let a = CommandLine.arguments
guard a.count == 8, let w = Int(a[2]), let h = Int(a[3]),
      let r = Double(a[4]), let g = Double(a[5]), let b = Double(a[6]) else {
    FileHandle.standardError.write("usage: mkimg out w h r g b text\n".data(using: .utf8)!)
    exit(2)
}
let text = a[7]
let cs = CGColorSpaceCreateDeviceRGB()
guard let ctx = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8,
                          bytesPerRow: 0, space: cs,
                          bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else {
    FileHandle.standardError.write("ctx fail\n".data(using: .utf8)!); exit(1)
}
ctx.setFillColor(CGColor(red: r/255, green: g/255, blue: b/255, alpha: 1))
ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
let font = CTFontCreateWithName("Helvetica-Bold" as CFString, 56, nil)
let white = CGColor(red: 1, green: 1, blue: 1, alpha: 1)
let attrs: [CFString: Any] = [
    kCTFontAttributeName: font,
    kCTForegroundColorAttributeName: white,
]
let attr = NSAttributedString(string: text, attributes: attrs as [NSAttributedString.Key: Any])
let line = CTLineCreateWithAttributedString(attr)
let bounds = CTLineGetBoundsWithOptions(line, .useGlyphPathBounds)
ctx.textPosition = CGPoint(x: (Double(w) - bounds.width) / 2 - bounds.origin.x,
                           y: (Double(h) - bounds.height) / 2 - bounds.origin.y)
CTLineDraw(line, ctx)
guard let img = ctx.makeImage() else { exit(1) }
let url = URL(fileURLWithPath: a[1]) as CFURL
guard let dest = CGImageDestinationCreateWithURL(url, UTType.png.identifier as CFString, 1, nil) else { exit(1) }
CGImageDestinationAddImage(dest, img, nil)
if !CGImageDestinationFinalize(dest) { exit(1) }
SWIFT

echo "== generating color+text images =="
swift "$TMP/mkimg.swift" "$OUT_DIR/blue.png" 320 240 21 101 192 "$BLUE_WORD"
swift "$TMP/mkimg.swift" "$OUT_DIR/red.png" 320 240 198 40 40 "$RED_WORD"

# --- sidecar manifest the test reads ------------------------------------------
python3 - "$OUT_DIR/manifest.json" "$PHRASE" "$CLIP_MS" "$BLUE_WORD" "$RED_WORD" <<PYEOF
import json, sys
out, phrase, clip_ms, blue_word, red_word = sys.argv[1:6]
asr_words = $ASR_WORDS
manifest = {
    "video": {
        "file": "speech.mp4",
        "phrase": phrase,
        "asr_words": asr_words,
        "duration_ms": int(clip_ms),
        "title": "M3 e2e speech clip",
    },
    "images": [
        {"file": "blue.png", "color": "blue", "word": blue_word,
         "title": "M3 e2e blue image"},
        {"file": "red.png", "color": "red", "word": red_word,
         "title": "M3 e2e red image"},
    ],
}
with open(out, "w") as f:
    json.dump(manifest, f, indent=2, sort_keys=True)
    f.write("\n")
PYEOF

echo
echo "== fixtures written to $OUT_DIR =="
ls -l "$OUT_DIR"
echo
TOTAL="$(find "$OUT_DIR" -type f -exec ls -l {} + | awk '{s+=$5} END{print s}')"
echo "total fixture bytes: ${TOTAL}"
echo "known phrase:        \"$PHRASE\""
echo "asr search words:    $ASR_WORDS"
echo "blue image word:     $BLUE_WORD   red image word: $RED_WORD"
