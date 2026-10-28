#!/bin/sh
# q2-airplay for the Shanling Q2: the app (static Go for 32-bit little-endian MIPS, soft float) and
# cliairplay (build-cliairplay.sh), staged with the card files in OUT/.airplay and zipped as a release
# ships it, OUT/q2-airplay-vX.Y-SHA.zip, to unzip at the card's root (or for System settings -> Apps).
#   contrib/shanlingq2/build.sh FIRMWARE_ZIP [OUT]
# FIRMWARE_ZIP is Shanling's "Q2 Firmware V1.32.zip": cliairplay links against the Q2's own libraries.
# Needs Go 1.22 or later, and what build-cliairplay.sh needs.
set -eu
[ $# -ge 1 ] || { echo "usage: $0 FIRMWARE_ZIP [OUT]" >&2; exit 2; }
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
OUT=${2:-$ROOT/target/shanlingq2}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
VERSION=$(sed -n 's/^const version = "q2-\(v[0-9.]*\)"$/\1/p' "$ROOT/cmd/q2-airplay/main.go")
[ -n "$VERSION" ] || { echo "build.sh: no version constant in cmd/q2-airplay/main.go" >&2; exit 1; }
SHA=$(git -C "$ROOT" rev-parse -q --short=7 --verify HEAD || echo local)

"$HERE/build-cliairplay.sh" "$1" "$OUT/cliairplay" >/dev/null

R="$OUT/.airplay"
rm -rf "$R"
mkdir -p "$R"
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
    go build -trimpath -ldflags='-s -w -buildid=' -o "$R/q2-airplay" ./cmd/q2-airplay
cp "$OUT/cliairplay/cliairplay" "$R/cliairplay"
cp "$HERE/launcher.sh" "$R/run"
cp "$HERE/q2app" "$R/"
cp "$ROOT/LICENSE" "$R/"
{
    echo "cliairplay (GPL-3.0) is built by contrib/shanlingq2/build-cliairplay.sh of"
    echo "https://github.com/group-a-study/q2-airplay at $(git -C "$ROOT" rev-parse -q --verify HEAD || echo "$SHA"),"
    echo "from these sources (repository, commit):"
    sed -n 's/^checkout [^ ]* \([^ ]*\) \([0-9a-f]*\)$/  \1 \2/p' "$HERE/build-cliairplay.sh"
} > "$R/SOURCES"
chmod 755 "$R/q2-airplay" "$R/cliairplay" "$R/run"
chmod 644 "$R/q2app" "$R/LICENSE" "$R/SOURCES"
rm -f "$OUT"/q2-airplay-*.zip
python3 -I "$HERE/package.py" "$R" "$OUT/q2-airplay-$VERSION-$SHA.zip"
