#!/bin/sh
# Builds the two images docker-compose.yml runs: voipd and Asterisk.
#
#   deploy/build.sh
#       Native build for this machine, e.g. on the Raspberry Pi itself
#       (works with the classic builder; the first Asterisk build takes
#       about 45 minutes on a Pi 4, later builds use the cache).
#
#   deploy/build.sh --platform linux/arm64 [--out DIR]
#       Cross build on a faster machine with docker buildx and QEMU
#       (docker run --privileged --rm tonistiigi/binfmt --install arm64).
#       The images are saved to DIR (default: .) as tar files; copy them to
#       the Pi and load them with: docker load -i voipd-<arch>.tar
#
# Image names follow docker-compose.yml; override with VOIPD_IMAGE and
# ASTERISK_IMAGE.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
VOIPD_IMAGE=${VOIPD_IMAGE:-voipd:dev}
ASTERISK_IMAGE=${ASTERISK_IMAGE:-voipd-asterisk:22.11.0}
PLATFORM=
OUT=.

while [ $# -gt 0 ]; do
	case "$1" in
	--platform) PLATFORM=$2; shift 2 ;;
	--out) OUT=$2; shift 2 ;;
	-h | --help) sed -n '2,17p' "$0"; exit 0 ;;
	*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
done

if [ -z "$PLATFORM" ]; then
	echo "building $ASTERISK_IMAGE and $VOIPD_IMAGE for $(uname -m)"
	docker build -t "$ASTERISK_IMAGE" "$ROOT/deploy/asterisk"
	docker build -t "$VOIPD_IMAGE" "$ROOT"
	exit 0
fi

if ! docker buildx version >/dev/null 2>&1; then
	echo "cross builds need docker buildx; without it, run deploy/build.sh on the target machine" >&2
	exit 1
fi
arch=${PLATFORM#*/}
arch=${arch%%/*}
mkdir -p "$OUT"
echo "building $ASTERISK_IMAGE and $VOIPD_IMAGE for $PLATFORM into $OUT"
docker buildx build --platform "$PLATFORM" -t "$ASTERISK_IMAGE" \
	-o "type=docker,dest=$OUT/voipd-asterisk-$arch.tar" "$ROOT/deploy/asterisk"
docker buildx build --platform "$PLATFORM" -t "$VOIPD_IMAGE" \
	-o "type=docker,dest=$OUT/voipd-$arch.tar" "$ROOT"
echo "done: copy $OUT/voipd-asterisk-$arch.tar and $OUT/voipd-$arch.tar to the Pi and docker load -i each"
