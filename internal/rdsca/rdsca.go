// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Package rdsca embeds the CAs that sign RDS and Aurora server certificates,
// which are not in OS trust stores. Embedding lets dbctl verify exactly as the
// controller does. Refresh with hack/update-rds-ca.sh.
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

// Pool returns the system roots plus every RDS CA.
func Pool() (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// No system store (a scratch image) is not fatal.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, errors.New("rdsca: the embedded RDS CA bundle contains no certificates")
	}
	return pool, nil
}

// Bundle returns a copy of the embedded PEM bundle.
func Bundle() []byte { return append([]byte(nil), bundle...) }
