// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

package rdsauth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func testProvider(t *testing.T) (p *TokenProvider, clock *time.Time, mints *int) {
	t.Helper()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	calls := 0
	p = NewWithCredentials(Config{
		Host:   "example.abc123.us-east-1.rds.amazonaws.com",
		Port:   5432,
		Region: "us-east-1",
		User:   "db_provisioner",
	}, credentials.NewStaticCredentialsProvider("AKIAEXAMPLE", "secret", ""))
	p.now = func() time.Time { return now }
	p.build = func(ctx context.Context, endpoint, region, user string, creds aws.CredentialsProvider) (string, error) {
		calls++
		return fmt.Sprintf("token-%d", calls), nil
	}
	return p, &now, &calls
}

func TestTokenIsCachedWithinItsLifetime(t *testing.T) {
	p, clock, mints := testProvider(t)
	ctx := context.Background()

	if _, err := p.Token(ctx); err != nil {
		t.Fatalf("Token returned error: %v", err)
	}

	*clock = clock.Add(5 * time.Minute)
	if _, err := p.Token(ctx); err != nil {
		t.Fatalf("Token returned error: %v", err)
	}

	if *mints != 1 {
		t.Errorf("minted %d tokens five minutes in, want 1 -- the cached token is still valid", *mints)
	}
}

func TestTokenIsRefreshedBeforeExpiry(t *testing.T) {
	p, clock, mints := testProvider(t)
	ctx := context.Background()

	if _, err := p.Token(ctx); err != nil {
		t.Fatalf("Token returned error: %v", err)
	}

	// Inside the refresh margin: RDS would still accept the cached token, but a
	// connection opened now could outlive it.
	*clock = clock.Add(tokenLifetime - refreshMargin + time.Second)
	if _, err := p.Token(ctx); err != nil {
		t.Fatalf("Token returned error: %v", err)
	}

	if *mints != 2 {
		t.Errorf("minted %d tokens inside the refresh margin, want 2", *mints)
	}
}

func TestEndpointJoinsHostAndPort(t *testing.T) {
	p := NewWithCredentials(Config{
		Host:   "aurora.cluster-abc.us-east-1.rds.amazonaws.com",
		Port:   5432,
		Region: "us-east-1",
		User:   "db_provisioner",
	}, aws.AnonymousCredentials{})

	if got, want := p.Endpoint(), "aurora.cluster-abc.us-east-1.rds.amazonaws.com:5432"; got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
}

func TestPortDefaultsTo5432(t *testing.T) {
	p := NewWithCredentials(Config{
		Host:   "db.us-east-1.rds.amazonaws.com",
		Region: "us-east-1",
		User:   "db_provisioner",
	}, aws.AnonymousCredentials{})

	if got, want := p.Endpoint(), "db.us-east-1.rds.amazonaws.com:5432"; got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
}
