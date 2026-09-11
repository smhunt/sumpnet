package platform

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// newRegistry builds a per-process registry (not the global default) with the
// standard Go/process collectors and a build-info gauge.
func newRegistry(cfg Config) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sumpnet_build_info",
		Help: "Build information for the running service; the value is always 1.",
	}, []string{"service", "version", "commit"})
	buildInfo.WithLabelValues(cfg.Service, Version, Commit).Set(1)
	reg.MustRegister(buildInfo)
	return reg
}
