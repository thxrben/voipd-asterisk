#!/bin/sh
# Renders the voice prompts listed in $1 (name|text per line) with Piper TTS
# into $2 as 16 kHz mono WAV (.wav16, which Asterisk plays natively),
# trimmed of leading/trailing silence, plus a beep.
#   render.sh prompts/de.txt /out/sounds/voipd   (needs piper, its voice, sox)
set -eu
list=$1 out=$2
model=${PIPER_MODEL:-/opt/piper/voice.onnx}
raw=$(mktemp -d)
mkdir -p "$out"

grep -v '^[[:space:]]*#' "$list" | grep '|' | while IFS='|' read -r name text; do
    esc=$(printf '%s' "$text" | sed 's/\\/\\\\/g; s/"/\\"/g')
    printf '{"text": "%s", "output_file": "%s/%s.wav"}\n' "$esc" "$raw" "$name"
done | /opt/piper/piper --model "$model" --json-input --sentence_silence 0.15 >/dev/null 2>"$raw/piper.log" ||
    { cat "$raw/piper.log"; exit 1; }

n=0
for f in "$raw"/*.wav; do
    name=$(basename "$f" .wav)
    # 16 kHz mono 16 bit; trim silence at both ends, keep 80 ms of air.
    sox "$f" -r 16000 -c 1 -b 16 -t wav "$out/$name.wav16" \
        silence 1 0.02 0.3% reverse silence 1 0.02 0.3% reverse pad 0.08 0.08
    n=$((n + 1))
done
sox -n -r 16000 -c 1 -b 16 -t wav "$out/beep.wav16" synth 0.35 sine 1000 vol 0.5 fade 0.01 0.35 0.01
expected=$(grep -v '^[[:space:]]*#' "$list" | grep -c '|')
[ "$n" -eq "$expected" ] || { echo "rendered $n of $expected prompts"; exit 1; }
echo "rendered $n prompts into $out"
rm -rf "$raw"
