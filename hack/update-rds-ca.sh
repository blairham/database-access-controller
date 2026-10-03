#!/usr/bin/env bash
# Refresh the embedded RDS CA bundle (internal/rdsca/global-bundle.pem).
#
# AWS publishes one bundle covering every commercial region's RDS and Aurora
# CAs. Every CA in it is a root valid until 2061 or later, so this needs
# running only when AWS adds a CA -- a new region, or a new CA family.
# `go test ./internal/rdsca/` afterwards checks the download is what it should be.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
curl -fsS https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
  -o "$root/internal/rdsca/global-bundle.pem"
grep -c 'BEGIN CERTIFICATE' "$root/internal/rdsca/global-bundle.pem" | xargs printf '%s certificates\n'
