package weather

import (
	"math"
	"testing"
	"time"
)

var base = time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC)

func up(at time.Duration, fcnt, tips int64, intervalS int32) GaugeUplink {
	return GaugeUplink{At: base.Add(at), FCnt: fcnt, TipCount: tips, MMPerTip: 0.2, IntervalS: intervalS}
}

func row(start time.Duration, intervalS int32, mm float64) RainRow {
	return RainRow{Start: base.Add(start), IntervalS: intervalS, MM: mm}
}

func TestDeriveGaugeRainfall(t *testing.T) {
	m := time.Minute
	reset := func(u GaugeUplink) GaugeUplink { u.CounterReset = true; return u }
	fault := func(u GaugeUplink) GaugeUplink { u.SensorFault = true; return u }
	prev := up(0, 10, 100, 900)
	cases := []struct {
		name string
		prev *GaugeUplink
		ups  []GaugeUplink
		want []RainRow
	}{
		{
			name: "first uplink ever without reset is only a baseline",
			ups:  []GaugeUplink{up(0, 0, 57, 900), up(15*m, 1, 57, 900)},
			want: []RainRow{row(0, 900, 0)},
		},
		{
			name: "first uplink after boot counts from boot",
			ups:  []GaugeUplink{reset(up(2*m, 0, 0, 120)), up(17*m, 1, 0, 900)},
			want: []RainRow{row(0, 120, 0), row(2*m, 900, 0)},
		},
		{
			name: "tipping every 5 minutes",
			prev: &prev,
			ups:  []GaugeUplink{up(5*m, 11, 103, 300), up(10*m, 12, 110, 300)},
			want: []RainRow{row(0, 300, 0.6000000000000001), row(5*m, 300, 1.4000000000000001)},
		},
		{
			name: "tips after a dry-cadence interval fell in its last 5 minutes",
			prev: &prev,
			ups:  []GaugeUplink{up(10*m, 11, 101, 600)},
			want: []RainRow{row(0, 300, 0), row(5*m, 300, 0.2)},
		},
		{
			name: "a lost uplink widens the interval and keeps the rain",
			prev: &prev,
			// fcnt 11 at 5 min was lost; the node's interval_s covers only 5 min.
			ups:  []GaugeUplink{up(10*m, 12, 104, 300)},
			want: []RainRow{row(0, 600, 0.8)},
		},
		{
			name: "counter reset counts from boot but not before the previous uplink",
			prev: &prev,
			ups:  []GaugeUplink{reset(up(15*m, 0, 3, 240)), reset(up(40*m, 0, 2, 3600))},
			want: []RainRow{row(11*m, 240, 0.6000000000000001), row(15*m, 1200, 0), row(35*m, 300, 0.4)},
		},
		{
			name: "a counter that went backwards without the flag is a reset",
			prev: &prev,
			ups:  []GaugeUplink{up(5*m, 11, 4, 300)},
			want: []RainRow{row(0, 300, 0.8)},
		},
		{
			name: "sensor fault yields nothing but still serves as the baseline",
			prev: &prev,
			ups:  []GaugeUplink{fault(up(5*m, 11, 130, 300)), up(10*m, 12, 131, 300)},
			want: []RainRow{row(5*m, 300, 0.2)},
		},
		{
			name: "duplicate time is skipped",
			prev: &prev,
			ups:  []GaugeUplink{up(0, 11, 100, 900)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveGaugeRainfall(tc.prev, tc.ups)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				g, w := got[i], tc.want[i]
				if !g.Start.Equal(w.Start) || g.IntervalS != w.IntervalS || math.Abs(g.MM-w.MM) > 1e-9 {
					t.Errorf("row %d = %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

func TestDeriveGaugeRainfallIsContiguous(t *testing.T) {
	// A gauge alternating dry 15-minute and wet 5-minute uplinks covers time
	// without gaps or overlaps, and the depth adds up.
	var ups []GaugeUplink
	at, tips := time.Duration(0), int64(0)
	ups = append(ups, GaugeUplink{At: base, TipCount: 0, MMPerTip: 0.2, IntervalS: 60, CounterReset: true})
	for i := 1; i <= 40; i++ {
		step, add := 15*time.Minute, int64(0)
		if i%4 != 0 {
			step, add = 5*time.Minute, int64(i%3)
		}
		at += step
		tips += add
		ups = append(ups, GaugeUplink{At: base.Add(at), FCnt: int64(i), TipCount: tips, MMPerTip: 0.2, IntervalS: int32(step / time.Second)})
	}
	rows := DeriveGaugeRainfall(nil, ups)
	var mm float64
	end := base.Add(-time.Minute)
	for _, r := range rows {
		if !r.Start.Equal(end) {
			t.Fatalf("row %+v does not start at the previous end %v", r, end)
		}
		end = r.Start.Add(time.Duration(r.IntervalS) * time.Second)
		mm += r.MM
	}
	if !end.Equal(ups[len(ups)-1].At) || math.Abs(mm-float64(tips)*0.2) > 1e-9 {
		t.Errorf("covered to %v with %.1f mm, want %v and %.1f mm", end, mm, ups[len(ups)-1].At, float64(tips)*0.2)
	}
}
