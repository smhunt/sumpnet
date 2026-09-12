// Package alerts is the alerts service: it turns node alarms, heartbeat
// conditions and detector output into alert rows with a raise/ack/resolve
// lifecycle, notifies an operator by email, and serves alerts.v1.AlertService.
// It is the single writer of the alerts table.
package alerts

import (
	"strings"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
)

// Severity maps a code to its fixed severity.
func Severity(code alertsv1.AlertCode) alertsv1.AlertSeverity {
	switch code {
	case alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH,
		alertsv1.AlertCode_ALERT_CODE_DRY_RUN,
		alertsv1.AlertCode_ALERT_CODE_CONTINUOUS_RUN,
		alertsv1.AlertCode_ALERT_CODE_OUTAGE_RISK:
		return alertsv1.AlertSeverity_ALERT_SEVERITY_CRITICAL
	case alertsv1.AlertCode_ALERT_CODE_MAINS_LOST,
		alertsv1.AlertCode_ALERT_CODE_SENSOR_FAULT,
		alertsv1.AlertCode_ALERT_CODE_SHORT_CYCLING,
		alertsv1.AlertCode_ALERT_CODE_LOW_BATTERY,
		alertsv1.AlertCode_ALERT_CODE_OFFLINE:
		return alertsv1.AlertSeverity_ALERT_SEVERITY_WARNING
	}
	return alertsv1.AlertSeverity_ALERT_SEVERITY_INFO
}

// CodeName is the short enum name: ALERT_CODE_FLOAT_HIGH → FLOAT_HIGH.
func CodeName(code alertsv1.AlertCode) string {
	return strings.TrimPrefix(code.String(), "ALERT_CODE_")
}

// SeverityName is the short enum name: ALERT_SEVERITY_CRITICAL → CRITICAL.
func SeverityName(s alertsv1.AlertSeverity) string {
	return strings.TrimPrefix(s.String(), "ALERT_SEVERITY_")
}

// ParseSeverity accepts info|warning|critical (any case).
func ParseSeverity(s string) (alertsv1.AlertSeverity, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return alertsv1.AlertSeverity_ALERT_SEVERITY_INFO, true
	case "warning", "warn":
		return alertsv1.AlertSeverity_ALERT_SEVERITY_WARNING, true
	case "critical", "crit":
		return alertsv1.AlertSeverity_ALERT_SEVERITY_CRITICAL, true
	}
	return alertsv1.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED, false
}
