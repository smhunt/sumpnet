package bridge

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the collectors shared by every bridge.
type Metrics struct {
	Messages      *prometheus.CounterVec   // result=decoded|dropped
	Drops         *prometheus.CounterVec   // reason
	QueueDepth    *prometheus.GaugeVec     // kind
	BatchRows     *prometheus.HistogramVec // kind
	SubmitSeconds *prometheus.HistogramVec // kind
	SubmitRetries *prometheus.CounterVec   // kind
	Accepted      *prometheus.CounterVec   // kind
	Duplicates    *prometheus.CounterVec   // kind
	TimeFallback  prometheus.Counter
	MQTTConnected prometheus.Gauge
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_bridge_mqtt_messages_total", Help: "MQTT messages received, by outcome."}, []string{"result"}),
		Drops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_bridge_drops_total", Help: "Messages dropped before submission, by reason."}, []string{"reason"}),
		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sumpnet_bridge_queue_depth", Help: "Messages waiting in a batcher queue."}, []string{"kind"}),
		BatchRows: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sumpnet_bridge_batch_rows", Help: "Rows per submitted batch.",
			Buckets: []float64{1, 10, 50, 100, 250, 500, 1000}}, []string{"kind"}),
		SubmitSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sumpnet_bridge_submit_seconds", Help: "Ingest round trip per batch.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14)}, []string{"kind"}),
		SubmitRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_bridge_submit_retries_total", Help: "Batch submissions retried."}, []string{"kind"}),
		Accepted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_bridge_accepted_total", Help: "Rows ingest reported as new."}, []string{"kind"}),
		Duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_bridge_duplicates_total", Help: "Rows ingest reported as already stored."}, []string{"kind"}),
		TimeFallback: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sumpnet_bridge_time_fallback_total", Help: "Messages without an event time that used receive time."}),
		MQTTConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sumpnet_bridge_mqtt_connected", Help: "1 while the MQTT session is up."}),
	}
	reg.MustRegister(m.Messages, m.Drops, m.QueueDepth, m.BatchRows, m.SubmitSeconds, m.SubmitRetries, m.Accepted, m.Duplicates, m.TimeFallback, m.MQTTConnected)
	return m
}
