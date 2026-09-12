package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCORS(t *testing.T) {
	const dash = "https://dev.ecoworks.ca:3034"
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := CORS([]string{dash}, next)
	tests := []struct {
		name       string
		method     string
		origin     string
		preflight  bool
		wantCode   int
		wantAllow  string
		wantMethod bool
	}{
		{name: "no origin passes through", method: http.MethodGet, wantCode: http.StatusTeapot},
		{name: "allowed origin", method: http.MethodGet, origin: dash, wantCode: http.StatusTeapot, wantAllow: dash},
		{name: "other origin gets no allow header", method: http.MethodGet, origin: "https://evil.example", wantCode: http.StatusTeapot},
		{name: "allowed preflight", method: http.MethodOptions, origin: dash, preflight: true, wantCode: http.StatusNoContent, wantAllow: dash, wantMethod: true},
		{name: "refused preflight", method: http.MethodOptions, origin: "https://evil.example", preflight: true, wantCode: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/v1/segments", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.preflight {
				r.Header.Set("Access-Control-Request-Method", "GET")
				r.Header.Set("Access-Control-Request-Headers", "authorization")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Errorf("code = %d, want %d", w.Code, tc.wantCode)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("allow-origin = %q, want %q", got, tc.wantAllow)
			}
			if tc.wantMethod && w.Header().Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" {
				t.Errorf("preflight headers = %v", w.Header())
			}
			if tc.origin != "" && w.Header().Get("Vary") == "" {
				t.Error("missing Vary: Origin")
			}
		})
	}
}

// writeSelfSigned writes a throwaway localhost certificate.
func writeSelfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestServeREST(t *testing.T) {
	cert, key := writeSelfSigned(t)
	tests := []struct {
		name      string
		cert, key string
		scheme    string
		wantTLS   bool
	}{
		{name: "plain", scheme: "http"},
		{name: "tls", cert: cert, key: key, scheme: "https", wantTLS: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
			go func() { done <- serveREST(ctx, lis, h, tc.cert, tc.key, time.Second) }()
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
			resp, err := client.Get(tc.scheme + "://" + lis.Addr().String() + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || (resp.TLS != nil) != tc.wantTLS {
				t.Fatalf("status %d, tls %v", resp.StatusCode, resp.TLS != nil)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("serveREST = %v", err)
			}
		})
	}
}

func TestLoopback(t *testing.T) {
	tests := []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{Port: 9092}, "127.0.0.1:9092"},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 9092}, "127.0.0.1:9092"},
		{&net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 9092}, "10.0.0.5:9092"},
	}
	for _, tc := range tests {
		if got := loopback(tc.addr); got != tc.want {
			t.Errorf("loopback(%v) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func TestTLSFiles(t *testing.T) {
	cert, key := writeSelfSigned(t)
	tests := []struct {
		name     string
		cfg      Config
		wantCert string
		wantErr  bool
	}{
		{name: "plain", cfg: Config{}},
		{name: "valid pair", cfg: Config{TLSCertFile: cert, TLSKeyFile: key}, wantCert: cert},
		{name: "missing pair fails", cfg: Config{TLSCertFile: "/nope/cert.pem", TLSKeyFile: "/nope/key.pem"}, wantErr: true},
		{name: "missing pair falls back", cfg: Config{TLSCertFile: "/nope/cert.pem", TLSKeyFile: "/nope/key.pem", TLSFallbackHTTP: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _, err := tlsFiles(tc.cfg)
			if (err != nil) != tc.wantErr || c != tc.wantCert {
				t.Fatalf("tlsFiles = %q, %v", c, err)
			}
		})
	}
}
