#!/usr/bin/env bash
# Copyright (C) 2026 Sovrenix Inc.
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Enforces the SPDX header convention from CLAUDE.md on every source file,
# including tests and generated code. A convention that is not checked is a
# convention that decays.

set -euo pipefail

copyright='Copyright (C) 2026 Sovrenix Inc.'
spdx='SPDX-License-Identifier: GPL-3.0-or-later'
fail=0

while IFS= read -r f; do
  head -n 5 "$f" | grep -qF "$copyright" || { echo "missing copyright: $f"; fail=1; }
  head -n 5 "$f" | grep -qF "$spdx"      || { echo "missing SPDX id:   $f"; fail=1; }
done < <(git ls-files --cached --others --exclude-standard '*.go' '*.sh' | sort -u)

if [ "$fail" -ne 0 ]; then
  echo
  echo "Every source file starts with:"
  echo "    // $copyright"
  echo "    // $spdx"
  exit 1
fi
echo "headers ok: $(git ls-files --cached --others --exclude-standard '*.go' '*.sh' | sort -u | wc -l) files"
