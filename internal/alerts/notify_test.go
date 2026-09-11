package alerts

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
)

// fakeSMTP is a scripted responder: it records every client command.
type fakeSMTP struct {
	conn       net.Conn
	commands   []string
	offerTLS   bool
	tlsConfig  *tls.Config // when set, STARTTLS upgrades the connection
	implicit   bool
	dataBody   string
	acceptAuth bool
}

func (f *fakeSMTP) serve(t *testing.T) {
	t.Helper()
	conn := f.conn
	if f.implicit {
		conn = tls.Server(conn, f.tlsConfig)
	}
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	r := bufio.NewReader(conn)
	w("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.commands = append(f.commands, line)
		cmd := strings.ToUpper(strings.Fields(line + " ")[0])
		switch cmd {
		case "EHLO":
			if f.offerTLS {
				w("250-fake")
				w("250-STARTTLS")
				w("250 AUTH PLAIN")
			} else {
				w("250-fake")
				w("250 AUTH PLAIN")
			}
		case "STARTTLS":
			w("220 go ahead")
			conn = tls.Server(conn, f.tlsConfig)
			r = bufio.NewReader(conn)
			w = func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		case "AUTH":
			if f.acceptAuth {
				w("235 ok")
			} else {
				w("535 no")
			}
		case "MAIL", "RCPT":
			w("250 ok")
		case "DATA":
			w("354 go")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				body.WriteString(l)
			}
			f.dataBody = body.String()
			w("250 queued")
		case "QUIT":
			w("221 bye")
			// Keep reading so the client's TLS close-notify has a peer (net.Pipe is synchronous).
			for {
				if _, err := r.ReadString('\n'); err != nil {
					_ = conn.Close()
					return
				}
			}
		default:
			w("500 what")
		}
	}
}

func selfSigned(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp.test"}, DNSNames: []string{"smtp.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	client := &tls.Config{RootCAs: pool, ServerName: "smtp.test", MinVersion: tls.VersionTLS12}
	return server, client
}

func sample() Message {
	home := uuid.MustParse("3f2a1b4c-0000-4000-8000-000000000001")
	return Message{
		AlertID: uuid.MustParse("11111111-2222-4333-8444-555555555555"), Event: "raised",
		Code: alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH, Severity: alertsv1.AlertSeverity_ALERT_SEVERITY_CRITICAL,
		DeviceID: "70b3d57ed0000007", HomeID: &home, SegmentID: "seg-03",
		At: time.Date(2026, 4, 15, 7, 41, 0, 0, time.UTC), Body: "Node float switch reports high water (level 350 mm)",
	}
}

func run(t *testing.T, n *SMTPNotifier, f *fakeSMTP) error {
	t.Helper()
	client, server := net.Pipe()
	f.conn = server
	done := make(chan struct{})
	go func() { f.serve(t); close(done) }()
	n.Dial = func(context.Context, string) (net.Conn, error) { return client, nil }
	err := n.Notify(context.Background(), sample())
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server did not finish")
	}
	return err
}

func TestSMTPStartTLSWithAuth(t *testing.T) {
	serverTLS, clientTLS := selfSigned(t)
	f := &fakeSMTP{offerTLS: true, tlsConfig: serverTLS, acceptAuth: true}
	n := &SMTPNotifier{cfg: SMTPConfig{Host: "smtp.test", Port: 587, User: "apikey", Password: "s3cret", From: "alerts@example.com", To: []string{"ops@example.com"}, TLS: "auto", Timeout: 5 * time.Second}, TLSConfig: clientTLS}
	if err := run(t, n, f); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	want := []string{"EHLO sumpnet", "STARTTLS", "EHLO sumpnet", "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00apikey\x00s3cret")), "MAIL FROM:<alerts@example.com>", "RCPT TO:<ops@example.com>", "DATA", "QUIT"}
	for i, w := range want {
		if i >= len(f.commands) || !strings.HasPrefix(strings.ToUpper(f.commands[i]), strings.ToUpper(w)) {
			t.Fatalf("command %d = %q, want prefix %q (all: %q)", i, get(f.commands, i), w, f.commands)
		}
	}
	if !strings.Contains(f.dataBody, "Subject: [sumpnet CRITICAL] FLOAT_HIGH raised — seg-03 / home 3f2a1b4c") {
		t.Errorf("subject missing:\n%s", f.dataBody)
	}
	if !strings.Contains(f.dataBody, "Message-ID: <alert-11111111-2222-4333-8444-555555555555.raised@sumpnet.local>") {
		t.Errorf("message id missing:\n%s", f.dataBody)
	}
	if !strings.Contains(f.dataBody, "Time (UTC):      2026-04-15T07:41:00Z") || !strings.Contains(f.dataBody, "level 350 mm") {
		t.Errorf("body missing fields:\n%s", f.dataBody)
	}
}

func TestSMTPImplicitTLSOn465(t *testing.T) {
	serverTLS, clientTLS := selfSigned(t)
	f := &fakeSMTP{implicit: true, tlsConfig: serverTLS, acceptAuth: true}
	n := &SMTPNotifier{cfg: SMTPConfig{Host: "smtp.test", Port: 465, User: "u", Password: "p", From: "a@example.com", To: []string{"b@example.com"}, TLS: "auto", Timeout: 5 * time.Second}, TLSConfig: clientTLS}
	if err := run(t, n, f); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(f.commands) == 0 || !strings.HasPrefix(f.commands[0], "EHLO") || f.commands[1] != "AUTH PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00u\x00p")) {
		t.Fatalf("commands = %q", f.commands)
	}
}

func TestSMTPRefusesToDowngrade(t *testing.T) {
	f := &fakeSMTP{offerTLS: false, acceptAuth: true}
	n := &SMTPNotifier{cfg: SMTPConfig{Host: "smtp.test", Port: 587, From: "a@example.com", To: []string{"b@example.com"}, TLS: "auto", Timeout: 5 * time.Second}}
	err := run(t, n, f)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a STARTTLS refusal", err)
	}
	for _, c := range f.commands {
		if strings.HasPrefix(c, "MAIL") || strings.HasPrefix(c, "AUTH") {
			t.Fatalf("client proceeded without TLS: %q", f.commands)
		}
	}
}

func TestSMTPPlainForLoopbackOnly(t *testing.T) {
	f := &fakeSMTP{acceptAuth: true}
	n := &SMTPNotifier{cfg: SMTPConfig{Host: "127.0.0.1", Port: 1025, From: "a@example.com", To: []string{"b@example.com"}, TLS: "none", Timeout: 5 * time.Second}}
	if err := run(t, n, f); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	bad := SMTPConfig{Host: "smtp.example.com", Port: 25, Password: "x", From: "a@example.com", To: []string{"b@example.com"}, TLS: "none"}
	if err := bad.validate(); err == nil {
		t.Fatal("TLS=none with a password to a remote host must be rejected")
	}
}

func TestLogNotifierAndConfig(t *testing.T) {
	if _, ok := NewNotifier(SMTPConfig{}, nil).(LogNotifier); !ok {
		t.Fatal("empty host must yield LogNotifier")
	}
	t.Setenv("SMTP_HOST", "mail.smtp2go.com")
	t.Setenv("SMTP_PORT", "2525")
	t.Setenv("SMTP_FROM", "alerts@example.com")
	t.Setenv("ALERTS_TO", "a@example.com, b@example.com")
	c, err := SMTPConfigFromEnv()
	if err != nil || c.Port != 2525 || len(c.To) != 2 || (&SMTPNotifier{cfg: c}).mode() != "starttls" {
		t.Fatalf("config = %+v, %v", c, err)
	}
}

func get(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}
