package privacy

import "testing"

func f(v float64) *float64 { return &v }

func TestVisible(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want bool
	}{{0, false}, {1, false}, {2, false}, {3, true}, {60, true}} {
		if got := Visible(tc.n); got != tc.want {
			t.Errorf("Visible(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestAggregateStorm(t *testing.T) {
	tests := []struct {
		name  string
		homes []HomeStorm
		want  SegmentStorm
	}{
		{"empty is suppressed", nil, SegmentStorm{SegmentID: "s", Suppressed: true}},
		{"two homes suppressed, numbers hidden", []HomeStorm{
			{HomeID: "a", VolumeL: 100, LagMin: f(10)}, {HomeID: "b", VolumeL: 300, LagMin: f(20)},
		}, SegmentStorm{SegmentID: "s", HomesReporting: 2, Suppressed: true}},
		{"duplicate rows count once", []HomeStorm{
			{HomeID: "a", VolumeL: 100}, {HomeID: "a", VolumeL: 100}, {HomeID: "b", VolumeL: 300},
		}, SegmentStorm{SegmentID: "s", HomesReporting: 2, Suppressed: true}},
		{"three homes, odd median, nil lag skipped", []HomeStorm{
			{HomeID: "a", VolumeL: 100, LagMin: f(10), RecessionMin: f(60)},
			{HomeID: "b", VolumeL: 200, LagMin: f(30), RecessionMin: f(90)},
			{HomeID: "c", VolumeL: 300, LagMin: nil, RecessionMin: f(120)},
		}, SegmentStorm{SegmentID: "s", HomesReporting: 3, LoadLPerHome: 200, MedianLagMin: 20, MedianRecessionMin: 90}},
		{"no thresholds reached", []HomeStorm{
			{HomeID: "a", VolumeL: 30}, {HomeID: "b", VolumeL: 30}, {HomeID: "c", VolumeL: 30},
		}, SegmentStorm{SegmentID: "s", HomesReporting: 3, LoadLPerHome: 30}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateStorm("s", tc.homes); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAggregateStatus(t *testing.T) {
	if got := AggregateStatus("s", 2, 12, 1); got != (SegmentStatus{SegmentID: "s", HomesReporting: 2, Suppressed: true}) {
		t.Errorf("suppressed status leaked: %+v", got)
	}
	if got := AggregateStatus("s", 4, 12, 1); got != (SegmentStatus{SegmentID: "s", HomesReporting: 4, CyclesPerHour: 3, ActiveAlerts: 1}) {
		t.Errorf("status = %+v", got)
	}
}

func TestMedianDoesNotMutate(t *testing.T) {
	xs := []float64{3, 1, 2, 4}
	if m := median(xs); m != 2.5 {
		t.Errorf("median = %v", m)
	}
	if xs[0] != 3 {
		t.Errorf("input mutated: %v", xs)
	}
}
