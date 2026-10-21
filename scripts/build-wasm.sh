#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
root=$(pwd)
sha256=0e94d961c31ce7d33e8b7ce4ac6fdbec83ee5658784eed69eb7fce300729d717
# Under the matrix runner (an absolute TAILTERM_VERIFICATION_CACHE with its Go
# caches set) the build uses the runner's Go caches and keeps the tarball in
# the harness cache by its sha256. Otherwise, the checkout's own .build.
case "${TAILTERM_VERIFICATION_CACHE:-}" in
  /*) shared=${GOCACHE:+${GOMODCACHE:+1}} ;;
  *) shared= ;;
esac
if [ -n "$shared" ]; then
  tarball="$TAILTERM_VERIFICATION_CACHE/downloads/$sha256.tar.gz"
else
  export GOPATH="$root/.build/go" GOCACHE="$root/.build/cache"
  tarball="$root/.build/tailscale.tar.gz"
fi
source="$root/.build/tailscale-1.102.3"
copy= download= extract= licenses=
cleanup() {
  for temporary in "$copy" "$download" "$extract" "$licenses"; do
    if [ -n "$temporary" ]; then rm -rf "$temporary"; fi
  done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
verify() {
  if command -v shasum >/dev/null 2>&1; then
    printf '%s  %s\n' "$sha256" "$1" | shasum -a 256 -c - >/dev/null
  else
    printf '%s  %s\n' "$sha256" "$1" | sha256sum -c - >/dev/null
  fi
}
state=extracted-present
if [ ! -d "$source" ]; then
  mkdir -p .build "$(dirname "$tarball")"
  # The tarball is read only through a verified private copy, so a cached file
  # changed after its check is never extracted. A bad cached file is replaced.
  copy="$root/.build/tailscale-copy.$$.tmp"
  state=cached
  if [ ! -f "$tarball" ] || ! { cp "$tarball" "$copy" && verify "$copy"; }; then
    rm -f "$tarball" "$copy"
    download="$tarball.$$.tmp"
    curl -fL https://codeload.github.com/tailscale/tailscale/tar.gz/refs/tags/v1.102.3 -o "$download"
    verify "$download" || { echo "build-wasm: the downloaded tarball failed its sha256 check" >&2; exit 1; }
    # Copied before it is published: a peer replacing a bad file never takes ours.
    cp "$download" "$copy"
    verify "$copy"
    mv -f "$download" "$tarball"
    state=downloaded
  fi
  extract="$root/.build/tailscale-extract.$$.tmp"
  mkdir "$extract"
  tar -xzf "$copy" -C "$extract"
  if [ ! -d "$source" ]; then mv "$extract/tailscale-1.102.3" "$source"; fi
fi
echo "build-wasm: GOCACHE=$(go env GOCACHE) GOMODCACHE=$(go env GOMODCACHE) tarball=$tarball ($state)" >&2
cp wasm/upstream_js.go "$source/cmd/tsconnect/wasm/wasm_js.go"
cp wasm/ssh_js.go "$source/cmd/tsconnect/wasm/ssh_js.go"
cp wasm/upload_js.go "$source/cmd/tsconnect/wasm/upload_js.go"
cp wasm/sftp_js.go "$source/cmd/tsconnect/wasm/sftp_js.go"
cp wasm/fixture_js.go "$source/cmd/tsconnect/wasm/fixture_js.go"
cd "$source"
GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' -o "$root/wasm/tailserve.wasm" ./cmd/tsconnect/wasm
if [ "${1:-}" = "--test" ]; then
  GOOS=js GOARCH=wasm go build -tags=tailserve_test -trimpath -ldflags='-s -w' -o "$root/.build/test.wasm" ./cmd/tsconnect/wasm
fi
cat "$(go env GOROOT)/lib/wasm/wasm_exec.js" > "$root/wasm/wasm_exec.js"
cat "$(go env GOROOT)/LICENSE" > "$root/wasm/LICENSE.go"
# Each module's licence texts are copied into the checkout beside a copy of
# the module list they were read for, so packaging never depends on a Go
# module cache that may since have been removed. Packaging fails closed on a
# module with none; the module list itself is still written last.
licenses="$root/.build/go-licenses.$$.tmp"
mkdir "$licenses"
GOOS=js GOARCH=wasm go list -deps -f '{{with .Module}}{{.Path}}|{{.Dir}}{{end}}' ./cmd/tsconnect/wasm > "$licenses/go-modules.txt"
sort -u "$licenses/go-modules.txt" | while IFS='|' read -r module dir || [ -n "$module" ]; do
  if [ -z "$module" ] || [ -z "$dir" ]; then continue; fi
  for name in LICENSE LICENSE.txt LICENSE.md COPYING NOTICE; do
    if [ -f "$dir/$name" ]; then
      mkdir -p "$licenses/modules/$module"
      # cat, not cp: module cache files are read-only, and cp keeps the mode.
      cat "$dir/$name" > "$licenses/modules/$module/$name"
    fi
  done
done
rm -rf "$root/.build/go-licenses"
mv "$licenses" "$root/.build/go-licenses"
licenses=
cat "$root/.build/go-licenses/go-modules.txt" > "$root/.build/go-modules.txt"
