package alerts

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

// Message is one notification: an alert being raised or resolved.
type Message struct {
	AlertID   uuid.UUID
	Event     string // "raised" | "resolved"
	Code      alertsv1.AlertCode
	Severity  alertsv1.AlertSeverity
	DeviceID  string
	HomeID    *uuid.UUID
	SegmentID string
	At        time.Time // event time
	Body      string
}

// Notifier delivers messages. Implementations must be safe for concurrent use.
type Notifier interface {
	Notify(ctx context.Context, m Message) error
}

// LogNotifier logs instead of sending (the default when SMTP_HOST is empty).
type LogNotifier struct{ Log *slog.Logger }

// Notify implements Notifier.
func (n LogNotifier) Notify(_ context.Context, m Message) error {
	n.Log.Info("alert notification (no SMTP configured)", "event", m.Event, "code", CodeName(m.Code), "severity", SeverityName(m.Severity), "device", m.DeviceID, "at", m.At)
	return nil
}

// RecordingNotifier captures messages for tests.
type RecordingNotifier struct {
	mu   sync.Mutex
	Sent []Message
	Err  error // returned from Notify when set
}

// Notify implements Notifier.
func (n *RecordingNotifier) Notify(_ context.Context, m Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.Err != nil {
		return n.Err
	}
	n.Sent = append(n.Sent, m)
	return nil
}

// Messages returns a copy of what was sent.
func (n *RecordingNotifier) Messages() []Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Message(nil), n.Sent...)
}

// SMTPConfig configures SMTPNotifier.
type SMTPConfig struct {
	Host     string        // SMTP_HOST; empty → LogNotifier
	Port     int           // SMTP_PORT (587)
	User     string        // SMTP_USER
	Password string        // SMTP_PASSWORD (a provider API key)
	From     string        // SMTP_FROM
	To       []string      // ALERTS_TO, comma-separated
	TLS      string        // SMTP_TLS: auto (465 → implicit, else STARTTLS) | starttls | implicit | none
	Timeout  time.Duration // SMTP_TIMEOUT (20s)
}

// SMTPConfigFromEnv reads the SMTP_* and ALERTS_TO variables.
func SMTPConfigFromEnv() (SMTPConfig, error) {
	c := SMTPConfig{Host: os.Getenv("SMTP_HOST"), Port: 587, User: os.Getenv("SMTP_USER"), Password: os.Getenv("SMTP_PASSWORD"), From: os.Getenv("SMTP_FROM"), TLS: "auto", Timeout: 20 * time.Second}
	if v, ok := os.LookupEnv("SMTP_PORT"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			return c, fmt.Errorf("SMTP_PORT: %q", v)
		}
		c.Port = n
	}
	if v, ok := os.LookupEnv("SMTP_TLS"); ok && v != "" {
		c.TLS = strings.ToLower(v)
	}
	if v, ok := os.LookupEnv("SMTP_TIMEOUT"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("SMTP_TIMEOUT: %w", err)
		}
		c.Timeout = d
	}
	for _, to := range strings.Split(os.Getenv("ALERTS_TO"), ",") {
		if to = strings.TrimSpace(to); to != "" {
			c.To = append(c.To, to)
		}
	}
	return c, c.validate()
}

func (c SMTPConfig) validate() error {
	if c.Host == "" {
		return nil
	}
	switch c.TLS {
	case "auto", "starttls", "implicit", "none":
	default:
		return fmt.Errorf("SMTP_TLS: %q (auto|starttls|implicit|none)", c.TLS)
	}
	if c.From == "" || len(c.To) == 0 {
		return errors.New("SMTP_FROM and ALERTS_TO are required when SMTP_HOST is set")
	}
	if c.TLS == "none" && c.Password != "" && !isLoopback(c.Host) {
		return errors.New("SMTP_TLS=none with a password is only allowed for loopback hosts")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewNotifier returns an SMTPNotifier, or a LogNotifier when no host is set.
func NewNotifier(cfg SMTPConfig, log *slog.Logger) Notifier {
	if cfg.Host == "" {
		return LogNotifier{Log: log}
	}
	return &SMTPNotifier{cfg: cfg}
}

// SMTPNotifier sends one message per connection using the standard library.
type SMTPNotifier struct {
	cfg  SMTPConfig
	Dial func(ctx context.Context, addr string) (net.Conn, error) // test hook
	// TLSConfig overrides the client TLS config (tests use a self-signed root).
	TLSConfig *tls.Config
}

func (n *SMTPNotifier) mode() string {
	if n.cfg.TLS != "auto" {
		return n.cfg.TLS
	}
	if n.cfg.Port == 465 {
		return "implicit"
	}
	return "starttls"
}

func (n *SMTPNotifier) tlsConfig() *tls.Config {
	if n.TLSConfig != nil {
		return n.TLSConfig
	}
	return &tls.Config{ServerName: n.cfg.Host, MinVersion: tls.VersionTLS12}
}

// Notify implements Notifier.
func (n *SMTPNotifier) Notify(ctx context.Context, m Message) error {
	dial := n.Dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: n.cfg.Timeout}).DialContext(ctx, "tcp", addr)
		}
	}
	addr := net.JoinHostPort(n.cfg.Host, strconv.Itoa(n.cfg.Port))
	conn, err := dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(n.cfg.Timeout))
	mode := n.mode()
	if mode == "implicit" {
		conn = tls.Client(conn, n.tlsConfig())
	}
	c, err := smtp.NewClient(conn, n.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: greeting: %w", err)
	}
	defer func() { _ = c.Close() }()
	if herr := c.Hello("sumpnet"); herr != nil {
		return fmt.Errorf("smtp: hello: %w", herr)
	}
	if mode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS (set SMTP_TLS=none only for a trusted LAN relay)")
		}
		if terr := c.StartTLS(n.tlsConfig()); terr != nil {
			return fmt.Errorf("smtp: starttls: %w", terr)
		}
	}
	if n.cfg.User != "" {
		if aerr := c.Auth(smtp.PlainAuth("", n.cfg.User, n.cfg.Password, n.cfg.Host)); aerr != nil {
			return fmt.Errorf("smtp: auth: %w", aerr)
		}
	}
	if merr := c.Mail(n.cfg.From); merr != nil {
		return fmt.Errorf("smtp: mail from: %w", merr)
	}
	for _, to := range n.cfg.To {
		if rerr := c.Rcpt(to); rerr != nil {
			return fmt.Errorf("smtp: rcpt %s: %w", to, rerr)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, werr := w.Write(FormatMessage(m, n.cfg.From, n.cfg.To, time.Now())); werr != nil {
		return fmt.Errorf("smtp: write: %w", werr)
	}
	if cerr := w.Close(); cerr != nil {
		return fmt.Errorf("smtp: end of data: %w", cerr)
	}
	if qerr := c.Quit(); qerr != nil {
		return fmt.Errorf("smtp: quit: %w", qerr)
	}
	return nil
}

// MessageID is deterministic per alert and event so a rare double send is
// deduplicated by the receiving mailbox.
func MessageID(m Message) string {
	return fmt.Sprintf("<alert-%s.%s@sumpnet.local>", m.AlertID, m.Event)
}

// Subject builds the subject line.
func Subject(m Message) string {
	where := "device " + m.DeviceID
	if m.HomeID != nil {
		where = fmt.Sprintf("home %s", m.HomeID.String()[:8])
	}
	if m.SegmentID != "" {
		where = m.SegmentID + " / " + where
	}
	return fmt.Sprintf("[sumpnet %s] %s %s — %s", SeverityName(m.Severity), CodeName(m.Code), m.Event, where)
}

// FormatMessage renders an RFC 5322 text/plain message (CRLF line endings).
func FormatMessage(m Message, from string, to []string, now time.Time) []byte {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteString("\r\n") }
	w("From: " + from)
	w("To: " + strings.Join(to, ", "))
	w("Date: " + now.UTC().Format(time.RFC1123Z))
	w("Subject: " + Subject(m))
	w("Message-ID: " + MessageID(m))
	w("MIME-Version: 1.0")
	w("Content-Type: text/plain; charset=utf-8")
	w("Content-Transfer-Encoding: 8bit")
	w("X-Sumpnet-Alert-Id: " + m.AlertID.String())
	w("X-Sumpnet-Code: " + CodeName(m.Code))
	w("")
	w(fmt.Sprintf("Alert %s: %s (%s)", m.Event, CodeName(m.Code), SeverityName(m.Severity)))
	w("")
	w("Time (UTC):      " + m.At.UTC().Format(time.RFC3339))
	if loc, err := time.LoadLocation("America/Toronto"); err == nil {
		w("Time (Toronto):  " + m.At.In(loc).Format("2006-01-02 15:04:05 MST"))
	}
	w("Device:          " + m.DeviceID)
	if m.HomeID != nil {
		w("Home:            " + m.HomeID.String())
	}
	if m.SegmentID != "" {
		w("Segment:         " + m.SegmentID)
	}
	w("")
	w(m.Body)
	w("")
	w("-- sumpnet alerts · alert id " + m.AlertID.String())
	return []byte(b.String())
}

// NotifierLoop delivers pending raise/resolve notifications.
type NotifierLoop struct {
	Queries     *sqlcgen.Queries // pool-bound
	Notifier    Notifier
	Metrics     *Metrics
	Log         *slog.Logger
	Interval    time.Duration // safety-net poll (30s)
	Retry       time.Duration // ALERTS_NOTIFY_RETRY (5m)
	MaxAttempts int32         // ALERTS_NOTIFY_MAX_ATTEMPTS (5)
	Wake        <-chan struct{}
}

// Run delivers until ctx ends.
func (l *NotifierLoop) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()
	for {
		if err := l.RunOnce(ctx); err != nil && ctx.Err() == nil {
			l.Log.Warn("notification round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-l.Wake:
		case <-ticker.C:
		}
	}
}

// RunOnce delivers everything currently pending.
func (l *NotifierLoop) RunOnce(ctx context.Context) error {
	params := sqlcgen.ListPendingRaiseNotificationsParams{MaxAttempts: l.MaxAttempts, RetrySeconds: l.Retry.Seconds(), Lim: 100}
	raises, err := l.Queries.ListPendingRaiseNotifications(ctx, params)
	if err != nil {
		return fmt.Errorf("pending raises: %w", err)
	}
	for _, r := range raises {
		msg := messageFor(r.Alert, r.SegmentID.String, "raised")
		state := l.deliver(ctx, msg)
		if merr := l.Queries.MarkRaiseNotification(ctx, sqlcgen.MarkRaiseNotificationParams{State: state, ID: r.Alert.ID}); merr != nil {
			return fmt.Errorf("mark raise: %w", merr)
		}
	}
	resolves, err := l.Queries.ListPendingResolveNotifications(ctx, sqlcgen.ListPendingResolveNotificationsParams(params))
	if err != nil {
		return fmt.Errorf("pending resolves: %w", err)
	}
	for _, r := range resolves {
		msg := messageFor(r.Alert, r.SegmentID.String, "resolved")
		state := l.deliver(ctx, msg)
		if merr := l.Queries.MarkResolveNotification(ctx, sqlcgen.MarkResolveNotificationParams{State: state, ID: r.Alert.ID}); merr != nil {
			return fmt.Errorf("mark resolve: %w", merr)
		}
	}
	return nil
}

func (l *NotifierLoop) deliver(ctx context.Context, m Message) string {
	start := time.Now()
	err := l.Notifier.Notify(ctx, m)
	l.Metrics.NotifySeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		l.Log.Warn("notification failed", "event", m.Event, "alert", m.AlertID, "err", err)
		l.Metrics.Notifications.WithLabelValues(m.Event, "failed").Inc()
		return StateFailed
	}
	l.Metrics.Notifications.WithLabelValues(m.Event, "sent").Inc()
	return StateSent
}

func messageFor(a sqlcgen.Alert, segment, event string) Message {
	m := Message{
		AlertID: a.ID, Event: event, Code: alertsv1.AlertCode(a.Code), Severity: alertsv1.AlertSeverity(a.Severity),
		DeviceID: a.DeviceID, SegmentID: segment, At: a.RaisedAt, Body: a.Message,
	}
	if a.HomeID.Valid {
		id := a.HomeID.UUID
		m.HomeID = &id
	}
	if event == "resolved" && a.ResolvedAt.Valid {
		m.At = a.ResolvedAt.Time
		m.Body = a.Message + "\r\nResolved: " + a.ResolveReason.String
	}
	return m
}
