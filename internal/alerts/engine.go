package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/watermark"
)

// Notification states (alerts.notify_state / resolve_notify_state).
const (
	StatePending    = "pending"
	StateSent       = "sent"
	StateSuppressed = "suppressed"
	StateFailed     = "failed"
	StateNone       = "none"
)

// Config tunes the engine and its rules.
type Config struct {
	Watermark         watermark.Config
	MinSeverity       alertsv1.AlertSeverity // ALERTS_EMAIL_MIN_SEVERITY: notify at or above (default warning)
	NotifyCooldown    time.Duration          // ALERTS_NOTIFY_COOLDOWN: suppress a re-raise email within this window (1h)
	NotifyResolve     string                 // ALERTS_NOTIFY_RESOLVE: critical | all | false
	LowBatteryMV      int32                  // ALERTS_LOW_BATTERY_MV (3500)
	LowBatteryClearMV int32                  // hysteresis (3700)
	RiseMinMM         int32                  // ALERTS_RISE_MIN_MM: total rise over the window for OUTAGE_RISK (10)
	RiseWindow        time.Duration          // readings considered for the rise (1h)
	OfflineAfter      time.Duration          // ALERTS_OFFLINE_AFTER: 0 disables the wall-clock sweep (1h)
}

// DefaultConfig is used when env vars are absent.
func DefaultConfig() Config {
	return Config{
		MinSeverity: alertsv1.AlertSeverity_ALERT_SEVERITY_WARNING, NotifyCooldown: time.Hour, NotifyResolve: "critical",
		LowBatteryMV: 3500, LowBatteryClearMV: 3700, RiseMinMM: 10, RiseWindow: time.Hour, OfflineAfter: time.Hour,
	}
}

// ConfigFromEnv overlays the ALERTS_* variables on the defaults.
func ConfigFromEnv() (Config, error) {
	c := DefaultConfig()
	wm, err := watermark.ConfigFromEnv("alerts")
	if err != nil {
		return c, err
	}
	c.Watermark = wm
	if v, ok := os.LookupEnv("ALERTS_EMAIL_MIN_SEVERITY"); ok {
		sev, ok := ParseSeverity(v)
		if !ok {
			return c, fmt.Errorf("ALERTS_EMAIL_MIN_SEVERITY: %q", v)
		}
		c.MinSeverity = sev
	}
	if v, ok := os.LookupEnv("ALERTS_NOTIFY_RESOLVE"); ok {
		v = strings.ToLower(v)
		if v != "critical" && v != "all" && v != "false" {
			return c, fmt.Errorf("ALERTS_NOTIFY_RESOLVE: %q", v)
		}
		c.NotifyResolve = v
	}
	for name, dst := range map[string]*time.Duration{"ALERTS_NOTIFY_COOLDOWN": &c.NotifyCooldown, "ALERTS_OFFLINE_AFTER": &c.OfflineAfter, "ALERTS_RISE_WINDOW": &c.RiseWindow} {
		if v, ok := os.LookupEnv(name); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return c, fmt.Errorf("%s: %w", name, err)
			}
			*dst = d
		}
	}
	for name, dst := range map[string]*int32{"ALERTS_LOW_BATTERY_MV": &c.LowBatteryMV, "ALERTS_RISE_MIN_MM": &c.RiseMinMM} {
		if v, ok := os.LookupEnv(name); ok {
			n, err := strconv.ParseInt(v, 10, 32)
			if err != nil {
				return c, fmt.Errorf("%s: %w", name, err)
			}
			*dst = int32(n)
		}
	}
	return c, nil
}

func (c Config) notifyResolve(sev alertsv1.AlertSeverity) bool {
	switch c.NotifyResolve {
	case "all":
		return true
	case "critical":
		return sev == alertsv1.AlertSeverity_ALERT_SEVERITY_CRITICAL
	}
	return false
}

// Metrics are the engine's collectors.
type Metrics struct {
	Raised        *prometheus.CounterVec // code, severity, source
	Resolved      *prometheus.CounterVec // code, reason
	Open          *prometheus.GaugeVec   // code
	Notifications *prometheus.CounterVec // event, result
	NotifySeconds prometheus.Histogram
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Raised: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_alerts_raised_total", Help: "Alerts opened."}, []string{"code", "severity", "source"}),
		Resolved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_alerts_resolved_total", Help: "Alerts resolved."}, []string{"code", "reason"}),
		Open: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sumpnet_alerts_open", Help: "Open alerts by code."}, []string{"code"}),
		Notifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_alerts_notifications_total", Help: "Notification attempts by outcome."}, []string{"event", "result"}),
		NotifySeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "sumpnet_alerts_notify_seconds", Help: "Time to deliver one notification.", Buckets: prometheus.ExponentialBuckets(0.01, 2, 12)}),
	}
	reg.MustRegister(m.Raised, m.Resolved, m.Open, m.Notifications, m.NotifySeconds)
	return m
}

type homeEntry struct {
	home    uuid.NullUUID
	segment string
	at      time.Time
}

// Engine raises and resolves alerts. All methods run inside the caller's
// transaction (q is bound to it).
type Engine struct {
	cfg   Config
	m     *Metrics
	log   *slog.Logger
	now   func() time.Time
	homes map[string]homeEntry
}

// NewEngine builds an Engine.
func NewEngine(cfg Config, m *Metrics, log *slog.Logger) *Engine {
	return &Engine{cfg: cfg, m: m, log: log, now: time.Now, homes: map[string]homeEntry{}}
}

func (e *Engine) homeOf(ctx context.Context, q *sqlcgen.Queries, dev string) (homeEntry, error) {
	if h, ok := e.homes[dev]; ok && e.now().Sub(h.at) < 5*time.Minute {
		return h, nil
	}
	row, err := q.GetDevicePitArea(ctx, dev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return homeEntry{}, fmt.Errorf("home of %s: %w", dev, err)
	}
	h := homeEntry{home: row.HomeID, segment: row.SegmentID.String, at: e.now()}
	e.homes[dev] = h
	return h, nil
}

// Raise opens an alert for (dev, code) or touches the open one. It returns
// true when a new row was created.
func (e *Engine) Raise(ctx context.Context, q *sqlcgen.Queries, dev string, code alertsv1.AlertCode, at time.Time, source, triggerKey, msg string) (bool, error) {
	c16 := int16(code) //nolint:gosec // enum ≤ 9
	open, err := q.GetOpenAlert(ctx, sqlcgen.GetOpenAlertParams{DeviceID: dev, Code: c16})
	switch {
	case err == nil:
		return false, q.TouchAlert(ctx, sqlcgen.TouchAlertParams{ID: open.ID, SeenAt: at})
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("get open alert: %w", err)
	}
	home, err := e.homeOf(ctx, q, dev)
	if err != nil {
		return false, err
	}
	sev := Severity(code)
	state := StatePending
	if sev < e.cfg.MinSeverity {
		state = StateSuppressed
	} else if last, lerr := q.LastNotifiedAt(ctx, sqlcgen.LastNotifiedAtParams{DeviceID: dev, Code: c16}); lerr == nil && last.Valid && e.now().Sub(last.Time) < e.cfg.NotifyCooldown {
		state = StateSuppressed
	}
	_, err = q.InsertAlert(ctx, sqlcgen.InsertAlertParams{
		DeviceID: dev, HomeID: home.home, Code: c16, Severity: int16(sev), //nolint:gosec // enum ≤ 3
		RaisedAt: at, Message: msg, Source: source, TriggerKey: triggerKey, NotifyState: state,
	})
	if err != nil {
		return false, fmt.Errorf("insert alert: %w", err)
	}
	e.m.Raised.WithLabelValues(CodeName(code), SeverityName(sev), source).Inc()
	e.log.Info("alert raised", "device", dev, "code", CodeName(code), "severity", SeverityName(sev), "at", at, "notify", state)
	return true, nil
}

// Resolve closes the open alert for (dev, code), if any.
func (e *Engine) Resolve(ctx context.Context, q *sqlcgen.Queries, dev string, code alertsv1.AlertCode, at time.Time, reason string) (bool, error) {
	_, err := q.ResolveOpenAlert(ctx, sqlcgen.ResolveOpenAlertParams{
		ResolvedAt: sql.NullTime{Time: at, Valid: true}, ResolveReason: pgtype.Text{String: reason, Valid: true},
		NotifyResolve: e.cfg.notifyResolve(Severity(code)),
		DeviceID:      dev, Code: int16(code), //nolint:gosec // enum ≤ 9
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve alert: %w", err)
	}
	e.m.Resolved.WithLabelValues(CodeName(code), reason).Inc()
	e.log.Info("alert resolved", "device", dev, "code", CodeName(code), "at", at, "reason", reason)
	return true, nil
}

// refreshOpenGauge updates the open-alerts gauge from the database.
func (e *Engine) refreshOpenGauge(ctx context.Context, q *sqlcgen.Queries) {
	rows, err := q.CountOpenAlertsByCode(ctx)
	if err != nil {
		return
	}
	e.m.Open.Reset()
	for _, r := range rows {
		e.m.Open.WithLabelValues(CodeName(alertsv1.AlertCode(r.Code))).Set(float64(r.N))
	}
}

func triggerKey(table, dev string, at time.Time, fcnt int64) string {
	return fmt.Sprintf("%s:%s:%s:%d", table, dev, at.UTC().Format(time.RFC3339), fcnt)
}
