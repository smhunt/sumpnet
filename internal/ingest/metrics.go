package ingest

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the ingest service's Prometheus collectors.
type Metrics struct {
	Rows              *prometheus.CounterVec   // kind, result=accepted|duplicate|rejected
	Rejects           *prometheus.CounterVec   // kind, reason
	BatchRows         *prometheus.HistogramVec // kind
	BatchSeconds      *prometheus.HistogramVec // kind
	StreamsActive     *prometheus.GaugeVec     // kind
	QueueBatches      *prometheus.GaugeVec     // kind
	PartitionsCreated *prometheus.CounterVec   // table
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Rows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_ingest_rows_total", Help: "Rows received by result.",
		}, []string{"kind", "result"}),
		Rejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_ingest_rejects_total", Help: "Rows rejected before storage, by reason.",
		}, []string{"kind", "reason"}),
		BatchRows: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sumpnet_ingest_batch_rows", Help: "Rows per stored batch.",
			Buckets: []float64{1, 10, 50, 100, 250, 500, 1000},
		}, []string{"kind"}),
		BatchSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sumpnet_ingest_batch_seconds", Help: "Store latency per batch.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
		}, []string{"kind"}),
		StreamsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sumpnet_ingest_streams_active", Help: "Open client streams.",
		}, []string{"kind"}),
		QueueBatches: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sumpnet_ingest_queue_batches", Help: "Batches waiting for the store.",
		}, []string{"kind"}),
		PartitionsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_ingest_partitions_created_total", Help: "Partitions created on demand for replays.",
		}, []string{"table"}),
	}
	reg.MustRegister(m.Rows, m.Rejects, m.BatchRows, m.BatchSeconds, m.StreamsActive, m.QueueBatches, m.PartitionsCreated)
	return m
}
