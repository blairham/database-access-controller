// Package rdsauth mints the short-lived IAM authentication tokens used to
// connect to RDS and Aurora as a database user, with no password anywhere.
//
// The Job this replaces shelled out to `aws rds generate-db-auth-token` after
// apk-installing the AWS CLI at container start, which cost roughly twenty
// seconds of startup per run and put a CLI in the image for one call. This is
// that call.
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

// tokenLifetime is how long RDS accepts a generated token. AWS fixes this at
// 15 minutes.
const tokenLifetime = 15 * time.Minute

// refreshMargin re-mints a token before it expires, so a connection opened just
// as the clock runs out does not fail.
const refreshMargin = 2 * time.Minute

// TokenProvider mints IAM auth tokens for one database user on one endpoint.
//
// Tokens are cached until shortly before they expire. A controller reconciles
// far more often than every fifteen minutes, and signing a token per reconcile
// is a needless STS-shaped dependency in the hot path.
type TokenProvider struct {
	endpoint string
	region   string
	user     string
	creds    aws.CredentialsProvider

	mu        sync.Mutex
	token     string
	expiresAt time.Time

	// now and build are injectable so the caching policy can be tested.
	// Comparing two token strings cannot show whether a token was re-minted:
	// BuildAuthToken signs with the real wall clock, so two mints in the same
	// second are byte-identical regardless of the cache.
	now   func() time.Time
	build func(ctx context.Context, endpoint, region, user string, creds aws.CredentialsProvider) (string, error)
}

// Config describes the target of an auth token.
type Config struct {
	// Host is the RDS or Aurora endpoint hostname.
	Host string

	// Port is the database port.
	Port int32

	// Region is the AWS region the instance lives in. The token is signed for
	// this region and is rejected anywhere else.
	Region string

	// User is the database user the token authenticates as.
	User string
}

// New returns a TokenProvider using the ambient AWS credentials.
//
// In cluster those credentials come from Pod Identity: the ServiceAccount is
// associated with an IAM role holding rds-db:connect on this user's dbuser ARN,
// and the SDK picks the association up from the container credentials endpoint
// with no configuration here.
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
