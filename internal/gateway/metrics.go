package gateway

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the api-gateway collectors.
type Metrics struct {
	Subscribers   prometheus.Gauge
	Updates       *prometheus.CounterVec // kind: segment_status, storm_event, alert
	Dropped       prometheus.Counter
	Rounds        *prometheus.CounterVec // result
	Notifications prometheus.Counter
	Auth          *prometheus.CounterVec // result: ok, expired, issuer, azp, signature, ...
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Subscribers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sumpnet_gateway_watch_subscribers", Help: "Open WatchNeighbourhood streams."}),
		Updates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_gateway_watch_updates_total", Help: "Neighbourhood updates produced, by kind."}, []string{"kind"}),
		Dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sumpnet_gateway_watch_dropped_total", Help: "Streams ended because the subscriber fell behind."}),
		Rounds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_gateway_watch_rounds_total", Help: "Neighbourhood recompute rounds, by result."}, []string{"result"}),
		Notifications: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sumpnet_gateway_watch_notifications_total", Help: "LISTEN wake-ups received."}),
		Auth: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_gateway_auth_total", Help: "Bearer token verifications, by result."}, []string{"result"}),
	}
	reg.MustRegister(m.Subscribers, m.Updates, m.Dropped, m.Rounds, m.Notifications, m.Auth)
	return m
}
