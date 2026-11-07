#!/usr/bin/env bash
# Writes the Gitleaks config CI scans with: .gitleaks.toml plus one case-insensitive rule per line of
# $GITLEAKS_PRIVATE_TERMS (regular expressions; blank lines and lines starting with # are skipped).
#   scripts/gitleaks-config.sh OUT.toml
set -euo pipefail
out=$1
q="'''" # TOML literal-string quotes, so a term needs no escaping
cp .gitleaks.toml "$out"
n=0
while IFS= read -r term || [ -n "$term" ]; do
  term=${term%$'\r'}
  [[ -z "${term// /}" || $term == \#* ]] && continue
  if [[ $term == *"$q"* ]]; then echo "a private term contains $q and can't be used" >&2; exit 2; fi
  n=$((n + 1))
  printf '\n[[rules]]\nid = "private-term-%d"\ndescription = "one of the maintainer'"'"'s private terms"\nregex = %s(?i)%s%s\n' \
    "$n" "$q" "$term" "$q" >>"$out"
done <<<"${GITLEAKS_PRIVATE_TERMS:-}"
echo "$n private-term rules" >&2
