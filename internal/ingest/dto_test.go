package ingest

import dto "github.com/prometheus/client_model/go"

// dtoMetric is a small alias so tests can read counter values.
type dtoMetric struct{ dto.Metric }
