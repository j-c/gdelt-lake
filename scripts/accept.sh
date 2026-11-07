#!/usr/bin/env bash
# Container check. Run on any Docker host from the repo: ./scripts/accept.sh
# Builds the images, runs the ingester once against a fake GDELT, checks the lake it wrote, and tears everything down.
# Prints PASS, or FAIL with logs.
set -euo pipefail
cd "$(dirname "$0")"
P="gdelt-lake-accept-$$"
DC=(docker compose -p "$P" -f accept.compose.yaml)
fail() { echo "== logs"; "${DC[@]}" --profile test logs --no-color --tail=200 || true; echo "FAIL: $1"; }
cleanup() { "${DC[@]}" --profile test down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== env"; docker version --format 'client {{.Client.Version}} / server {{.Server.Version}}'
echo "== build"; "${DC[@]}" --profile test build --pull || { fail build; exit 1; }
echo "== fake GDELT"; "${DC[@]}" up -d --wait --wait-timeout 60 fake-gdelt || { fail "fake GDELT"; exit 1; }
echo "== ingester (one run)"; "${DC[@]}" --profile test run --rm ingester || { fail ingester; exit 1; }
echo "== verify"; "${DC[@]}" --profile test run --rm verify || { fail verify; exit 1; }
echo "== licences"; ./image-licences.sh "$P-ingester" || { fail licences; exit 1; }
echo "PASS"
