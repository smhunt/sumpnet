package weather

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the weather service's collectors.
type Metrics struct {
	RainfallRows    *prometheus.CounterVec // source, result=written|unchanged|deleted
	GaugeResets     prometheus.Counter
	GaugeFaults     prometheus.Counter
	ECCCPolls       *prometheus.CounterVec // result=ok|error
	ECCCNullHours   prometheus.Counter
	ECCCLastSuccess prometheus.Gauge
}

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RainfallRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_weather_rainfall_rows_total", Help: "Rainfall rows derived, by source and outcome."}, []string{"source", "result"}),
		GaugeResets: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sumpnet_weather_gauge_counter_resets_total", Help: "Rain gauge uplinks whose tip counter restarted."}),
		GaugeFaults: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sumpnet_weather_gauge_sensor_faults_total", Help: "Rain gauge uplinks flagged sensor_fault (no rainfall derived)."}),
		ECCCPolls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sumpnet_weather_eccc_polls_total", Help: "ECCC GeoMet polls by result."}, []string{"result"}),
		ECCCNullHours: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sumpnet_weather_eccc_null_hours_total", Help: "ECCC hours without a precipitation amount (skipped)."}),
		ECCCLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sumpnet_weather_eccc_last_success_timestamp_seconds", Help: "Wall-clock time of the last successful ECCC poll."}),
	}
	reg.MustRegister(m.RainfallRows, m.GaugeResets, m.GaugeFaults, m.ECCCPolls, m.ECCCNullHours, m.ECCCLastSuccess)
	return m
}
