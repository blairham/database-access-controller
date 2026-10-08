// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/blairham/database-access-controller/internal/rdsca"
)

// testCA is a throwaway certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key}
}

func (ca testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// serverCert issues a certificate naming the given DNS names and IPs.
func (ca testCA) serverCert(t *testing.T, dnsNames []string, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeTLSServer accepts PostgreSQL connections on loopback, completes a TLS
// handshake and hangs up, reporting whether the client accepted the
// certificate.
func fakeTLSServer(t *testing.T, cert tls.Certificate) (port int32, handshake <-chan bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	results := make(chan bool, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// SSLRequest: int32 length 8, int32 code 80877103.
			if _, err := io.ReadFull(conn, make([]byte, 8)); err != nil {
				_ = conn.Close()
				continue
			}
			if _, err := conn.Write([]byte{'S'}); err != nil {
				_ = conn.Close()
				continue
			}
			srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			_ = srv.SetDeadline(time.Now().Add(5 * time.Second))
			select {
			case results <- srv.Handshake() == nil:
			default:
			}
			_ = srv.Close()
		}
	}()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return int32(addr.Port), results //nolint:gosec // a loopback port always fits
}

func TestConnectVerifiesServerCertificate(t *testing.T) {
	loopback := []net.IP{net.ParseIP("127.0.0.1")}
	trusted, untrusted := newTestCA(t), newTestCA(t)

	cases := []struct {
		cert    tls.Certificate
		name    string
		sslMode string
		accept  bool
	}{
		{
			name:    "verify-full accepts a trusted CA and matching name",
			sslMode: "verify-full",
			cert:    trusted.serverCert(t, nil, loopback),
			accept:  true,
		},
		{
			name:    "verify-full rejects an unknown CA",
			sslMode: "verify-full",
			cert:    untrusted.serverCert(t, nil, loopback),
			accept:  false,
		},
		{
			name:    "verify-full rejects a name mismatch",
			sslMode: "verify-full",
			cert:    trusted.serverCert(t, []string{"db.example.com"}, nil),
			accept:  false,
		},
		{
			name:    "verify-ca ignores the name",
			sslMode: "verify-ca",
			cert:    trusted.serverCert(t, []string{"db.example.com"}, nil),
			accept:  true,
		},
		{
			name:    "verify-ca rejects an unknown CA",
			sslMode: "verify-ca",
			cert:    untrusted.serverCert(t, nil, loopback),
			accept:  false,
		},
		// The default is what a DatabaseAccess without sslMode, and every
		// dbctl run without one, gets: it must verify.
		{
			name:    "empty sslMode defaults to verifying",
			sslMode: "",
			cert:    untrusted.serverCert(t, nil, loopback),
			accept:  false,
		},
		// Why the default changed: require accepts a certificate from anyone.
		{
			name:    "require accepts an unknown CA",
			sslMode: "require",
			cert:    untrusted.serverCert(t, nil, loopback),
			accept:  true,
		},
	}

	orig := rootCAs
	t.Cleanup(func() { rootCAs = orig })
	rootCAs = func() (*x509.CertPool, error) { return trusted.pool(), nil }

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, handshake := fakeTLSServer(t, tc.cert)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, err := Connect(ctx, ConnConfig{
				Host: "127.0.0.1", Port: port, Database: "db", User: "admin",
				SSLMode: tc.sslMode, Password: "unused",
			})
			if err == nil {
				_ = conn.Close(ctx)
				t.Fatal("Connect succeeded against a server that hangs up after TLS")
			}
			select {
			case ok := <-handshake:
				if ok != tc.accept {
					t.Fatalf("TLS handshake succeeded=%v, want %v (client error: %v)", ok, tc.accept, err)
				}
			case <-ctx.Done():
				t.Fatalf("server never saw a handshake (client error: %v)", err)
			}
		})
	}
}

// TestDefaultRootCAsTrustRDS pins that, out of the box, verify-full trusts the
// RDS CAs; the fake-server tests substitute rootCAs and cannot see this.
func TestDefaultRootCAsTrustRDS(t *testing.T) {
	pool, err := rootCAs()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(rdsca.Bundle())
	if block == nil {
		t.Fatal("the RDS bundle has no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("default rootCAs does not trust %s: %v", cert.Subject.CommonName, err)
	}
}
