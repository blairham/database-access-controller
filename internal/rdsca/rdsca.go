// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Package rdsca supplies the certificate authorities that sign RDS and Aurora
// server certificates.
//
// Those CAs are Amazon's own and are not in any operating system's trust
// store, so verifying an RDS server's certificate with the system roots alone
// fails. The bundle is embedded rather than read from the image so that dbctl,
// run from a workstation, verifies exactly as the controller does.
//
// Refresh it with hack/update-rds-ca.sh. Every CA in it is a root valid until
// 2061 or later, so the copy goes stale only when AWS adds a CA.
package rdsca

import (
	"crypto/x509"
	_ "embed"
	"errors"
)

// bundle is https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem.
//
//go:embed global-bundle.pem
var bundle []byte

// Pool returns the system roots plus every RDS CA. The system roots stay in so
// that verify-full also works against a self-managed PostgreSQL whose
// certificate comes from a public CA.
func Pool() (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// No system store (a scratch image, say) is not fatal: RDS servers
		// still verify against the embedded bundle.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, errors.New("rdsca: the embedded RDS CA bundle contains no certificates")
	}
	return pool, nil
}

// Bundle returns a copy of the embedded PEM bundle.
func Bundle() []byte { return append([]byte(nil), bundle...) }
