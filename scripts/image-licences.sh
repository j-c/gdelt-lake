#!/usr/bin/env bash
# Checks that an image carries the licences of what's compiled into it (see the Dockerfile).
#   scripts/image-licences.sh IMAGE
set -euo pipefail
image=$1
dir=$(mktemp -d)
cid=$(docker create "$image")
trap 'docker rm "$cid" >/dev/null; rm -rf "$dir"' EXIT
docker cp -q "$cid:/usr/share/licenses/gdelt-lake/." "$dir"
n=$(find "$dir" -type f | wc -l)
for f in go.dev/go/LICENSE github.com/apache/arrow-go/v18/NOTICE.txt go.duckdb.org/libduckdb/linux-amd64/LICENSE; do
  test -s "$dir/$f" || { echo "missing in the image: /usr/share/licenses/gdelt-lake/$f"; exit 1; }
done
echo "$n licence and notice files in /usr/share/licenses/gdelt-lake"
