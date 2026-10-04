#!/usr/bin/env bash
# Refresh the embedded RDS CA bundle (internal/rdsca/global-bundle.pem).
#
# Needed only when AWS adds a CA. Run `go test ./internal/rdsca/` afterwards.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
curl -fsS https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
  -o "$root/internal/rdsca/global-bundle.pem"
grep -c 'BEGIN CERTIFICATE' "$root/internal/rdsca/global-bundle.pem" | xargs printf '%s certificates\n'
