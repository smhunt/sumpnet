// Package authtest is an in-process stand-in for Clerk: an RSA signing key
// and a JWKS served over httptest TLS. Tests never call real Clerk.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/smhunt/sumpnet/internal/auth"
)

// Issuer serves a JWKS and signs Clerk-shaped session tokens.
type Issuer struct {
	Server *httptest.Server
	URL    string // the iss claim and JWKS base
	KID    string // kid of the default signing key

	mu   sync.Mutex
	keys map[string]*rsa.PrivateKey // published in the JWKS
	sign *rsa.PrivateKey
}

// NewIssuer starts a JWKS server publishing one RS256 key. Call Close.
func NewIssuer() (*Issuer, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	is := &Issuer{KID: "test-key-1", keys: map[string]*rsa.PrivateKey{}, sign: key}
	is.keys[is.KID] = key
	is.Server = httptest.NewTLSServer(http.HandlerFunc(is.serveJWKS))
	is.URL = is.Server.URL
	return is, nil
}

// NewKey generates an RSA key for signing test tokens.
func NewKey() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, 2048) }

// Close stops the JWKS server.
func (is *Issuer) Close() { is.Server.Close() }

// Client trusts the JWKS server's certificate.
func (is *Issuer) Client() *http.Client { return is.Server.Client() }

// Config points an auth.Config at this issuer.
func (is *Issuer) Config(authorizedParties ...string) auth.Config {
	return auth.Config{Issuer: is.URL, AuthorizedParties: authorizedParties, Leeway: time.Second}
}

// Publish adds key to the JWKS under kid (key rotation).
func (is *Issuer) Publish(kid string, key *rsa.PrivateKey) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.keys[kid] = key
}

type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (is *Issuer) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	set := struct {
		Keys []jwk `json:"keys"`
	}{Keys: []jwk{}}
	for kid, k := range is.keys {
		set.Keys = append(set.Keys, jwk{
			Kty: "RSA", Use: "sig", Alg: "RS256", Kid: kid,
			N: base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

// TokenOptions shape a token; the zero value (plus Subject/AZP) is a valid
// Clerk-like session token.
type TokenOptions struct {
	Subject   string
	AZP       string
	Issuer    string        // default: this issuer
	TTL       time.Duration // default 5 min; negative = already expired
	NotBefore time.Duration // nbf offset from now (positive = in the future)
	KID       string        // default: the published key's kid
	Key       *rsa.PrivateKey
	Method    jwt.SigningMethod // default RS256
	SignWith  any               // overrides the key entirely (HMAC secret, jwt.UnsafeAllowNoneSignatureType)
}

type clerkClaims struct {
	jwt.RegisteredClaims
	AuthorizedParty string `json:"azp,omitempty"`
	SessionID       string `json:"sid,omitempty"`
}

// Token signs a token.
func (is *Issuer) Token(o TokenOptions) (string, error) {
	now := time.Now()
	if o.TTL == 0 {
		o.TTL = 5 * time.Minute
	}
	if o.Issuer == "" {
		o.Issuer = is.URL
	}
	if o.KID == "" {
		o.KID = is.KID
	}
	if o.Method == nil {
		o.Method = jwt.SigningMethodRS256
	}
	c := clerkClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    o.Issuer,
			Subject:   o.Subject,
			IssuedAt:  jwt.NewNumericDate(now.Add(-10 * time.Second)),
			NotBefore: jwt.NewNumericDate(now.Add(o.NotBefore - 10*time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(o.TTL)),
		},
		AuthorizedParty: o.AZP,
		SessionID:       "sess_test",
	}
	t := jwt.NewWithClaims(o.Method, c)
	t.Header["kid"] = o.KID
	var key any = is.sign
	if o.Key != nil {
		key = o.Key
	}
	if o.SignWith != nil {
		key = o.SignWith
	}
	return t.SignedString(key)
}

// MustToken is Token that panics on error (test helper).
func (is *Issuer) MustToken(o TokenOptions) string {
	s, err := is.Token(o)
	if err != nil {
		panic(err)
	}
	return s
}
