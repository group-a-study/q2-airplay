#!/bin/sh
# Publish the card zip of main's pushed HEAD as release q2-vX.Y (cmd/q2-airplay/main.go's version):
#   contrib/shanlingq2/release.sh FIRMWARE_ZIP
# GitHub can't build cliairplay: it links against the Q2's own libraries, from Shanling's firmware
# ZIP, which isn't public. So the zip is built here and pushed, with the workflow that publishes it,
# as the one-commit branch release/q2-vX.Y; the release's tag points at main's commit.
set -eu
[ $# -ge 1 ] || { echo "usage: $0 FIRMWARE_ZIP" >&2; exit 2; }
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
cd "$ROOT"
[ -z "$(git status --porcelain)" ] || { echo "$0: commit first" >&2; exit 1; }
git fetch -q origin
src=$(git rev-parse HEAD)
git merge-base --is-ancestor "$src" origin/main || { echo "$0: push $src to main first" >&2; exit 1; }
tag=$(sed -n 's/^const version = "\(q2-v[0-9.]*\)"$/\1/p' cmd/q2-airplay/main.go)
work=$(mktemp -d "${TMPDIR:-/tmp}/q2-airplay-release.XXXXXX")
trap 'rm -rf "$work"' EXIT
"$HERE/build.sh" "$1" "$ROOT/target/shanlingq2" >/dev/null
r=$work/r
mkdir -p "$r/.github/workflows"
cp target/shanlingq2/q2-airplay-*.zip "$r/"
(cd "$r" && sha256sum q2-airplay-*.zip > SHA256SUMS)
echo "$src" > "$r/SOURCE"
echo "$tag" > "$r/TAG"
{
    echo "AirPlay for Q2 Pod: what the Q2 plays, on an AirPlay speaker (Sonos, HomePod, Apple TV, AirPlay 1"
    echo "speakers), chosen from Now Playing's AirPlay button. Needs a Q2 Pod build with AirPlay. See README.md."
    echo
    echo "Install: **System settings → Apps → AirPlay**, or unzip to the card's root with the Q2 off."
    echo
    echo "Built from $src. cliairplay's sources: .airplay/SOURCES in the zip."
    echo
    echo '```'
    cat "$r/SHA256SUMS"
    echo '```'
} > "$r/NOTES.md"
cat > "$r/.github/workflows/publish.yml" <<'YML'
name: Publish
# A push to a release/ branch publishes its zip as the release TAG names, on SOURCE (main's commit).
on:
  push:
    branches: ['release/**']

permissions:
  contents: write

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: sha256sum -c SHA256SUMS
      - env:
          GH_TOKEN: ${{ github.token }}
        run: >
          gh release create "$(cat TAG)" q2-airplay-*.zip -R "$GITHUB_REPOSITORY" --target "$(cat SOURCE)"
          --latest --title "q2-airplay ${GITHUB_REF_NAME#release/q2-}" --notes-file NOTES.md
YML
git -C "$r" init -q
git -C "$r" symbolic-ref HEAD "refs/heads/release/$tag"
git -C "$r" add -A -f
git -C "$r" -c user.name="$(git config user.name)" -c user.email="$(git config user.email)" \
    commit -q -m "Release $tag from $src"
git -C "$r" push -q "$(git remote get-url origin)" "release/$tag"
echo "Pushed release/$tag; in a minute or two it's https://github.com/group-a-study/q2-airplay/releases/tag/$tag"
