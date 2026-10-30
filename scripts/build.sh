#!/usr/bin/env bash
# Builds wowsimcli with the Simhive pool's commands for the platforms asked for:
#   scripts/build.sh <wowsims/forever checkout> <out dir> [goos/goarch ...]
# The checkout is a clone of github.com/wowsims/forever at the commit the pool pins. Its protobuf code is generated
# here (the repository does not commit it), the overlay's files are copied in beside the CLI's own, and each platform
# gets one binary, named wowsimcli-<goos>-<goarch>[.exe]. Needs go and protoc on PATH, and protoc-gen-go
# (google.golang.org/protobuf, not the deprecated github.com/golang/protobuf one).
set -euo pipefail

src=$(cd "$1" && pwd)
out=$(mkdir -p "$2" && cd "$2" && pwd)
shift 2
targets=("$@")
[ ${#targets[@]} -gt 0 ] || targets=("$(go env GOOS)/$(go env GOARCH)")
here=$(cd "$(dirname "$0")/.." && pwd)

cd "$src"
protoc -I=./proto \
  --go_opt=Mgoogle/protobuf/descriptor.proto=google.golang.org/protobuf/types/descriptorpb \
  --go_out=./sim/core ./proto/*.proto
cp -R "$here/overlay/." "$src/"

commit=$(git -C "$src" rev-parse HEAD)
for t in "${targets[@]}"; do
  goos=${t%/*} goarch=${t#*/}
  ext=; [ "$goos" = windows ] && ext=.exe
  echo "building $goos/$goarch from wowsims/forever $commit"
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build --tags=with_db -trimpath \
    -ldflags "-s -w -X main.Version=forever-${commit:0:12}" \
    -o "$out/wowsimcli-$goos-$goarch$ext" ./cmd/wowsimcli
done
