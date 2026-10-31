#!/usr/bin/env bash
# Portable product checks. No production services or credentials are needed.
set -euo pipefail
cd "$(dirname "$0")/.."
for tool in go node npm python3; do
  command -v "$tool" >/dev/null || { echo "Required tool missing: $tool" >&2; exit 1; }
done
python3 scripts/check_notices.py
go test -count=1 ./...
go test -tags headless -count=1 ./cmd/ferry ./web/console
go vet ./...
go build ./...
npm test --prefix web/console
python3 -B -m unittest discover -s skills/ferry/scripts -p 'test_*.py'
python3 -B -m unittest discover -s scripts/release -p 'test_*.py'
