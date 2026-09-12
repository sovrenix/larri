#!/usr/bin/env bash
# Copyright (C) 2026 Sovrenix Inc.
# SPDX-License-Identifier: GPL-3.0-or-later

set -euo pipefail

readonly max_bytes=$((10 * 1024 * 1024))
fail=0

while IFS= read -r -d '' file; do
  [[ -f "$file" ]] || continue
  bytes=$(wc -c < "$file")
  if (( bytes > max_bytes )); then
    printf 'file exceeds 10 MiB: %s (%s bytes)\n' "$file" "$bytes"
    fail=1
  fi
done < <(git ls-files --cached --others --exclude-standard -z)

if (( fail != 0 )); then
  exit 1
fi

echo "file sizes ok: <= 10 MiB"