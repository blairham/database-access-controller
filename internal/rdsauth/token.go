// SPDX-FileCopyrightText: 2026 Blair Hamilton
// SPDX-License-Identifier: Apache-2.0

// Package rdsauth mints the short-lived IAM authentication tokens used to
// connect to RDS and Aurora as a database user.
package rdsauth

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
)

// tokenLifetime is how long RDS accepts a generated token.
const tokenLifetime = 15 * time.Minute

// refreshMargin re-mints a token this long before it expires.
const refreshMargin = 2 * time.Minute

// TokenProvider mints IAM auth tokens for one database user on one endpoint,
// caching each until shortly before it expires.
type TokenProvider struct {
	endpoint string
	region   string
	user     string
	creds    aws.CredentialsProvider

	mu        sync.Mutex
	token     string
	expiresAt time.Time

	// now and build are injectable so the caching policy can be tested.
	now   func() time.Time
	build func(ctx context.Context, endpoint, region, user string, creds aws.CredentialsProvider) (string, error)
}

// Config describes the target of an auth token.
type Config struct {
	// Host is the RDS or Aurora endpoint hostname.
	Host string

	// Port is the database port.
	Port int32

	// Region is the AWS region the token is signed for.
	Region string

	// User is the database user the token authenticates as.
	User string
}

// New returns a TokenProvider using the ambient AWS credentials (in cluster,
// typically EKS Pod Identity or IRSA), which need rds-db:connect on the user.
func New(ctx context.Context, cfg Config) (*TokenProvider, error) {
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return NewWithCredentials(cfg, awsCfg.Credentials), nil
}

// NewWithCredentials returns a TokenProvider using the given credentials.
func NewWithCredentials(cfg Config, creds aws.CredentialsProvider) *TokenProvider {
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	return &TokenProvider{
		endpoint: net.JoinHostPort(cfg.Host, strconv.Itoa(int(port))),
		region:   cfg.Region,
		user:     cfg.User,
		creds:    creds,
		now:      time.Now,
		build: func(ctx context.Context, endpoint, region, user string, creds aws.CredentialsProvider) (string, error) {
			return auth.BuildAuthToken(ctx, endpoint, region, user, creds)
		},
	}
}

// Token returns a valid auth token, minting a new one if the cached token is
// missing or close to expiry.
func (p *TokenProvider) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token != "" && p.now().Before(p.expiresAt.Add(-refreshMargin)) {
		return p.token, nil
	}

	token, err := p.build(ctx, p.endpoint, p.region, p.user, p.creds)
	if err != nil {
		return "", fmt.Errorf("building RDS auth token for %s@%s: %w", p.user, p.endpoint, err)
	}

	p.token = token
	p.expiresAt = p.now().Add(tokenLifetime)
	return token, nil
}

// Endpoint returns the host:port the tokens are signed for.
func (p *TokenProvider) Endpoint() string { return p.endpoint }
