// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package rdsca

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// TestBundleIsRDSRoots pins that the embedded file is a non-trivial set of
// unexpired RDS root CAs, catching a bad hack/update-rds-ca.sh download.
func TestBundleIsRDSRoots(t *testing.T) {
	var n int
	for rest := bundle; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("certificate %d does not parse: %v", n, err)
		}
		if !strings.HasPrefix(cert.Subject.CommonName, "Amazon RDS") {
			t.Errorf("certificate %d is %q, not an Amazon RDS CA", n, cert.Subject.CommonName)
		}
		if !cert.IsCA {
			t.Errorf("certificate %d (%s) is not a CA", n, cert.Subject.CommonName)
		}
		if time.Now().After(cert.NotAfter) {
			t.Errorf("certificate %d (%s) expired %s", n, cert.Subject.CommonName, cert.NotAfter)
		}
		n++
	}
	if n < 50 {
		t.Fatalf("bundle holds %d certificates; the RDS global bundle has over a hundred", n)
	}
}

// TestPoolTrustsEveryRDSRoot checks the pool, not just the file: each RDS root
// must verify against what Pool returns.
func TestPoolTrustsEveryRDSRoot(t *testing.T) {
	pool, err := Pool()
	if err != nil {
		t.Fatal(err)
	}
	for rest := bundle; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
			t.Fatalf("%s does not verify against Pool(): %v", cert.Subject.CommonName, err)
		}
	}
}
