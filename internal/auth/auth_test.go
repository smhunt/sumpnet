package auth_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/smhunt/sumpnet/internal/auth"
	"github.com/smhunt/sumpnet/internal/auth/authtest"
)

const dashboard = "https://dev.ecoworks.ca:3034"

type hook struct {
	mu   sync.Mutex
	last string
}

func (h *hook) record(r string) { h.mu.Lock(); h.last = r; h.mu.Unlock() }
func (h *hook) get() string     { h.mu.Lock(); defer h.mu.Unlock(); return h.last }

func newVerifier(t *testing.T, is *authtest.Issuer, cfg auth.Config, h *hook) *auth.Verifier {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opts := []auth.Option{auth.WithHTTPClient(is.Client())}
	if h != nil {
		opts = append(opts, auth.WithResultHook(h.record))
	}
	v, err := auth.NewVerifier(ctx, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func issuer(t *testing.T) *authtest.Issuer {
	t.Helper()
	is, err := authtest.NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(is.Close)
	return is
}

func TestVerify(t *testing.T) {
	is := issuer(t)
	other, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	h := &hook{}
	v := newVerifier(t, is, is.Config(dashboard), h)

	tests := []struct {
		name       string
		opts       authtest.TokenOptions
		raw        string
		wantSub    string
		wantReason string
	}{
		{name: "valid", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard}, wantSub: "user_a", wantReason: "ok"},
		{name: "expired", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard, TTL: -time.Minute}, wantReason: "expired"},
		{name: "not yet valid", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard, NotBefore: time.Hour, TTL: 2 * time.Hour}, wantReason: "not_yet_valid"},
		{name: "wrong issuer", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard, Issuer: "https://evil.example.com"}, wantReason: "issuer"},
		{name: "wrong azp", opts: authtest.TokenOptions{Subject: "user_a", AZP: "https://evil.example.com"}, wantReason: "azp"},
		{name: "missing azp", opts: authtest.TokenOptions{Subject: "user_a"}, wantReason: "azp"},
		{name: "forged with the published kid", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard, Key: other}, wantReason: "signature"},
		{name: "unknown kid", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard, Key: other, KID: "nope"}, wantReason: "signature"},
		{name: "HS256", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard, Method: jwt.SigningMethodHS256, SignWith: []byte("secret")}, wantReason: "signature"},
		{name: "alg none", opts: authtest.TokenOptions{Subject: "user_a", AZP: dashboard, Method: jwt.SigningMethodNone, SignWith: jwt.UnsafeAllowNoneSignatureType}, wantReason: "signature"},
		{name: "missing subject", opts: authtest.TokenOptions{AZP: dashboard}, wantReason: "no_subject"},
		{name: "garbage", raw: "not.a.jwt", wantReason: "malformed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.raw
			if raw == "" {
				var err error
				if raw, err = is.Token(tc.opts); err != nil {
					t.Fatal(err)
				}
			}
			p, err := v.Verify(context.Background(), raw)
			if got := h.get(); got != tc.wantReason {
				t.Errorf("result = %q, want %q (err %v)", got, tc.wantReason, err)
			}
			if tc.wantSub == "" {
				if !errors.Is(err, auth.ErrInvalidToken) {
					t.Fatalf("err = %v, want ErrInvalidToken", err)
				}
				return
			}
			if err != nil || p.Subject != tc.wantSub {
				t.Fatalf("Verify = %+v, %v; want subject %q", p, err, tc.wantSub)
			}
		})
	}
}

func TestVerifyWithoutAuthorizedParties(t *testing.T) {
	is := issuer(t)
	v := newVerifier(t, is, is.Config(), nil)
	if _, err := v.Verify(context.Background(), is.MustToken(authtest.TokenOptions{Subject: "user_a"})); err != nil {
		t.Fatalf("token without azp and no parties configured: %v", err)
	}
}

func TestKeyRotationRefreshesJWKS(t *testing.T) {
	is := issuer(t)
	v := newVerifier(t, is, is.Config(dashboard), nil)
	next, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	is.Publish("test-key-2", next)
	raw := is.MustToken(authtest.TokenOptions{Subject: "user_b", AZP: dashboard, Key: next, KID: "test-key-2"})
	if p, err := v.Verify(context.Background(), raw); err != nil || p.Subject != "user_b" {
		t.Fatalf("rotated key: %+v, %v", p, err)
	}
}

func TestDisabled(t *testing.T) {
	v, err := auth.NewVerifier(context.Background(), auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled() {
		t.Fatal("empty config must disable auth")
	}
	if _, err := v.Verify(context.Background(), "x.y.z"); !errors.Is(err, auth.ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer x.y.z"))
	if _, err := v.Authenticate(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("token with auth disabled: %v", err)
	}
	if _, err := v.Authenticate(context.Background()); err != nil {
		t.Fatalf("anonymous with auth disabled: %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantErr  bool
		enabled  bool
		parties  int
		wantSkew time.Duration
	}{
		{name: "unset", env: map[string]string{}, wantSkew: 5 * time.Second},
		{name: "clerk", env: map[string]string{"CLERK_ISSUER": "https://clerk.example.com/", "CLERK_AUTHORIZED_PARTIES": "https://dev.ecoworks.ca:3034/, https://other.example"}, enabled: true, parties: 2, wantSkew: 5 * time.Second},
		{name: "skew", env: map[string]string{"CLERK_ISSUER": "https://clerk.example.com", "CLERK_CLOCK_SKEW": "30s"}, enabled: true, wantSkew: 30 * time.Second},
		{name: "loopback http", env: map[string]string{"CLERK_ISSUER": "http://127.0.0.1:9999"}, enabled: true, wantSkew: 5 * time.Second},
		{name: "plain http", env: map[string]string{"CLERK_ISSUER": "http://clerk.example.com"}, wantErr: true},
		{name: "http jwks", env: map[string]string{"CLERK_ISSUER": "https://clerk.example.com", "CLERK_JWKS_URL": "http://clerk.example.com/jwks"}, wantErr: true},
		{name: "parties without issuer stay disabled", env: map[string]string{"CLERK_AUTHORIZED_PARTIES": "https://x"}, parties: 1, wantSkew: 5 * time.Second},
		{name: "bad skew", env: map[string]string{"CLERK_ISSUER": "https://clerk.example.com", "CLERK_CLOCK_SKEW": "soon"}, wantErr: true},
		{name: "relative issuer", env: map[string]string{"CLERK_ISSUER": "clerk.example.com"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"CLERK_ISSUER", "CLERK_JWKS_URL", "CLERK_AUTHORIZED_PARTIES", "CLERK_CLOCK_SKEW"} {
				t.Setenv(k, tc.env[k])
			}
			c, err := auth.ConfigFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if c.Enabled() != tc.enabled || len(c.AuthorizedParties) != tc.parties || c.Leeway != tc.wantSkew {
				t.Fatalf("config = %+v", c)
			}
			if tc.name == "clerk" && (c.Issuer != "https://clerk.example.com" || c.AuthorizedParties[0] != "https://dev.ecoworks.ca:3034") {
				t.Fatalf("trailing slashes not trimmed: %+v", c)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	is := issuer(t)
	v := newVerifier(t, is, is.Config(dashboard), nil)
	valid := is.MustToken(authtest.TokenOptions{Subject: "user_a", AZP: dashboard})
	expired := is.MustToken(authtest.TokenOptions{Subject: "user_a", AZP: dashboard, TTL: -time.Minute})

	tests := []struct {
		name     string
		md       metadata.MD
		wantCode codes.Code
		wantSub  string
	}{
		{name: "anonymous", md: nil},
		{name: "bearer", md: metadata.Pairs("authorization", "Bearer "+valid), wantSub: "user_a"},
		{name: "lowercase scheme", md: metadata.Pairs("authorization", "bearer "+valid), wantSub: "user_a"},
		{name: "expired", md: metadata.Pairs("authorization", "Bearer "+expired), wantCode: codes.Unauthenticated},
		{name: "basic", md: metadata.Pairs("authorization", "Basic dXNlcjpwdw=="), wantCode: codes.Unauthenticated},
		{name: "empty bearer", md: metadata.Pairs("authorization", "Bearer "), wantCode: codes.Unauthenticated},
		{name: "two values", md: metadata.Pairs("authorization", "Bearer "+valid, "authorization", "Bearer "+valid), wantCode: codes.Unauthenticated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.md != nil {
				ctx = metadata.NewIncomingContext(ctx, tc.md)
			}
			var seen context.Context
			_, err := v.UnaryInterceptor()(ctx, nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
				seen = ctx
				return nil, nil
			})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v, want %v", status.Code(err), tc.wantCode)
			}
			if err != nil {
				return
			}
			p, ok := auth.FromContext(seen)
			if ok != (tc.wantSub != "") || p.Subject != tc.wantSub {
				t.Fatalf("principal = %+v, %t; want %q", p, ok, tc.wantSub)
			}
			_, rerr := auth.RequireOwner(seen)
			if (rerr == nil) != (tc.wantSub != "") || (rerr != nil && status.Code(rerr) != codes.Unauthenticated) {
				t.Fatalf("RequireOwner = %v", rerr)
			}

			// The stream interceptor must hand the same identity to the handler.
			ss := &fakeStream{ctx: ctx}
			err = v.StreamInterceptor()(nil, ss, &grpc.StreamServerInfo{}, func(_ any, s grpc.ServerStream) error {
				if p2, _ := auth.FromContext(s.Context()); p2.Subject != tc.wantSub {
					t.Errorf("stream principal = %+v, want %q", p2, tc.wantSub)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
		})
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeStream) Context() context.Context { return f.ctx }

// slowTransport delays every request so a JWKS refresh takes longer than a
// tiny rate-limit wait budget.
type slowTransport struct {
	rt    http.RoundTripper
	delay time.Duration
}

func (s slowTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	time.Sleep(s.delay)
	return s.rt.RoundTrip(r)
}

// TestKeyRotationWithSlowJWKS is the regression test for jwkset reusing the
// RateLimitWaitMax context for the refresh request: with a millisecond budget
// a rotated key was never fetched, however quickly the issuer published it.
func TestKeyRotationWithSlowJWKS(t *testing.T) {
	is := issuer(t)
	base := is.Client()
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	client := &http.Client{Transport: slowTransport{rt: rt, delay: 50 * time.Millisecond}, Timeout: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	v, err := auth.NewVerifier(ctx, is.Config(dashboard), auth.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	next, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	is.Publish("test-key-2", next)
	raw := is.MustToken(authtest.TokenOptions{Subject: "user_b", AZP: dashboard, Key: next, KID: "test-key-2"})
	if p, err := v.Verify(context.Background(), raw); err != nil || p.Subject != "user_b" {
		t.Fatalf("rotated key behind a slow JWKS: %+v, %v", p, err)
	}
}
