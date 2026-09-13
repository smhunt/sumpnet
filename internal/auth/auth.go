// Package auth verifies Clerk session tokens for the api-gateway (ADR 0006)
// and carries the caller's identity through gRPC request contexts.
//
// A token is accepted only if it is an RS256 JWT signed by a key in the
// issuer's JWKS, with the configured issuer, a subject, an unexpired exp, a
// passed nbf/iat (within a small clock-skew leeway) and, when authorized
// parties are configured, an azp claim naming one of them. The subject (the
// Clerk user id) is what home_owners.auth_subject links to a home.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

// Sentinel errors. Callers map all of them to UNAUTHENTICATED and never echo
// the detail to the client.
var (
	ErrInvalidToken = errors.New("auth: invalid token")
	ErrDisabled     = errors.New("auth: owner authentication is not configured")
)

// Config configures a Verifier. It comes from the environment only; the owner
// fills in real values in deploy/compose/.env.
type Config struct {
	// Issuer is the Clerk Frontend API URL (CLERK_ISSUER), e.g.
	// https://clerk.example.com or https://<slug>.clerk.accounts.dev. Empty
	// disables owner authentication: every token is rejected and only public
	// RPCs work.
	Issuer string
	// JWKSURL is CLERK_JWKS_URL; it defaults to Issuer + "/.well-known/jwks.json".
	JWKSURL string
	// AuthorizedParties is CLERK_AUTHORIZED_PARTIES (comma-separated origins,
	// e.g. https://dev.ecoworks.ca:3034). When set, the token's azp claim must
	// be one of them.
	AuthorizedParties []string
	// Leeway is CLERK_CLOCK_SKEW (default 5s), applied to exp, nbf and iat.
	Leeway time.Duration
}

// ConfigFromEnv reads CLERK_ISSUER, CLERK_JWKS_URL, CLERK_AUTHORIZED_PARTIES
// and CLERK_CLOCK_SKEW.
func ConfigFromEnv() (Config, error) {
	c := Config{
		Issuer:            strings.TrimRight(strings.TrimSpace(os.Getenv("CLERK_ISSUER")), "/"),
		JWKSURL:           strings.TrimSpace(os.Getenv("CLERK_JWKS_URL")),
		AuthorizedParties: splitList(os.Getenv("CLERK_AUTHORIZED_PARTIES")),
		Leeway:            5 * time.Second,
	}
	if v, ok := os.LookupEnv("CLERK_CLOCK_SKEW"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("CLERK_CLOCK_SKEW: invalid duration %q", v)
		}
		c.Leeway = d
	}
	return c, c.validate()
}

// Enabled reports whether owner authentication is configured.
func (c Config) Enabled() bool { return c.Issuer != "" }

func (c Config) jwksURL() string {
	if c.JWKSURL != "" {
		return c.JWKSURL
	}
	return c.Issuer + "/.well-known/jwks.json"
}

func (c Config) validate() error {
	if !c.Enabled() {
		return nil // CLERK_JWKS_URL / CLERK_AUTHORIZED_PARTIES are ignored without an issuer
	}
	if err := secureURL("CLERK_ISSUER", c.Issuer); err != nil {
		return err
	}
	return secureURL("CLERK_JWKS_URL", c.jwksURL())
}

// secureURL requires https, except for loopback hosts (local tests).
func secureURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("auth: %s must be an absolute URL, got %q", name, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return nil
		}
	}
	return fmt.Errorf("auth: %s must use https, got %q", name, raw)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Principal is an authenticated caller.
type Principal struct {
	Subject   string // Clerk user id (sub); home_owners.auth_subject
	SessionID string // Clerk session id (sid), for logs only
}

// claims are the Clerk session-token claims the gateway reads.
type claims struct {
	jwt.RegisteredClaims
	AuthorizedParty string `json:"azp,omitempty"`
	SessionID       string `json:"sid,omitempty"`
}

// Verifier validates Clerk session tokens.
type Verifier struct {
	cfg     Config
	keyfunc keyfunc.Keyfunc
	parser  *jwt.Parser
	onCheck func(result string)
}

// Option configures NewVerifier.
type Option func(*options)

type options struct {
	client  *http.Client
	now     func() time.Time
	onCheck func(result string)
}

// WithHTTPClient sets the client used to fetch the JWKS (tests use the
// httptest TLS client).
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithClock overrides the time source for exp/nbf/iat checks (tests).
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// WithResultHook is called once per verification with "ok" or a failure
// reason (metrics).
func WithResultHook(fn func(result string)) Option { return func(o *options) { o.onCheck = fn } }

// NewVerifier builds a Verifier. With authentication disabled it needs no
// network; otherwise it fetches the JWKS in the background (a failed first
// fetch is retried on the refresh interval and on an unknown kid) and ctx ends
// the refresh goroutine.
func NewVerifier(ctx context.Context, cfg Config, opts ...Option) (*Verifier, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	o := options{now: time.Now, onCheck: func(string) {}}
	for _, fn := range opts {
		fn(&o)
	}
	v := &Verifier{cfg: cfg, onCheck: o.onCheck}
	if !cfg.Enabled() {
		return v, nil
	}
	client := o.client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	k, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{cfg.jwksURL()}, keyfunc.Override{
		Client:      client,
		HTTPTimeout: 10 * time.Second,
		// Key rotation: an unknown kid may refresh the set at most once a
		// minute. jwkset reuses the RateLimitWaitMax context for the refresh
		// request itself, so it must cover a whole JWKS fetch. Limiter.Wait
		// fails at once when the next token is further away than this, so a
		// caller never queues behind the one-minute limit.
		RefreshUnknownKID: rate.NewLimiter(rate.Every(time.Minute), 1),
		RateLimitWaitMax:  10 * time.Second,
		RefreshInterval:   time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: jwks %s: %w", cfg.jwksURL(), err)
	}
	v.keyfunc = k
	v.parser = jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(cfg.Leeway),
		jwt.WithTimeFunc(o.now),
	)
	return v, nil
}

// Enabled reports whether tokens can be accepted at all.
func (v *Verifier) Enabled() bool { return v.cfg.Enabled() }

// Verify validates a raw JWT and returns its principal. Every failure wraps
// ErrInvalidToken or ErrDisabled.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if !v.Enabled() {
		v.onCheck("disabled")
		return Principal{}, ErrDisabled
	}
	var c claims
	if _, err := v.parser.ParseWithClaims(raw, &c, v.keyfunc.KeyfuncCtx(ctx)); err != nil {
		v.onCheck(reason(err))
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if c.Subject == "" {
		v.onCheck("no_subject")
		return Principal{}, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	if len(v.cfg.AuthorizedParties) > 0 && !slices.Contains(v.cfg.AuthorizedParties, c.AuthorizedParty) {
		v.onCheck("azp")
		return Principal{}, fmt.Errorf("%w: azp %q is not an authorized party", ErrInvalidToken, c.AuthorizedParty)
	}
	v.onCheck("ok")
	return Principal{Subject: c.Subject, SessionID: c.SessionID}, nil
}

// reason is a low-cardinality metric label for a parse failure.
func reason(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "expired"
	case errors.Is(err, jwt.ErrTokenNotValidYet), errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return "not_yet_valid"
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return "issuer"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid), errors.Is(err, jwt.ErrTokenUnverifiable):
		return "signature"
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "malformed"
	default:
		return "invalid"
	}
}
