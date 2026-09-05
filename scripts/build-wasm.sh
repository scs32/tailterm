#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
root=$(pwd)
export GOPATH="$root/.build/go" GOCACHE="$root/.build/cache"
source="$root/.build/tailscale-1.102.3"
if [ ! -d "$source" ]; then
  mkdir -p .build
  curl -fL https://codeload.github.com/tailscale/tailscale/tar.gz/refs/tags/v1.102.3 -o .build/tailscale.tar.gz
  printf '%s  %s\n' 0e94d961c31ce7d33e8b7ce4ac6fdbec83ee5658784eed69eb7fce300729d717 .build/tailscale.tar.gz | shasum -a 256 -c -
  tar -xzf .build/tailscale.tar.gz -C .build
fi
cp wasm/upstream_js.go "$source/cmd/tsconnect/wasm/wasm_js.go"
cp wasm/ssh_js.go "$source/cmd/tsconnect/wasm/ssh_js.go"
cp wasm/upload_js.go "$source/cmd/tsconnect/wasm/upload_js.go"
cp wasm/fixture_js.go "$source/cmd/tsconnect/wasm/fixture_js.go"
cd "$source"
GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' -o "$root/wasm/tailserve.wasm" ./cmd/tsconnect/wasm
if [ "${1:-}" = "--test" ]; then
  GOOS=js GOARCH=wasm go build -tags=tailserve_test -trimpath -ldflags='-s -w' -o "$root/.build/test.wasm" ./cmd/tsconnect/wasm
fi
cat "$(go env GOROOT)/lib/wasm/wasm_exec.js" > "$root/wasm/wasm_exec.js"
cat "$(go env GOROOT)/LICENSE" > "$root/wasm/LICENSE.go"
GOOS=js GOARCH=wasm go list -deps -f '{{with .Module}}{{.Path}}|{{.Dir}}{{end}}' ./cmd/tsconnect/wasm > "$root/.build/go-modules.txt"
