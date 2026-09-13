package sim

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/codec"
)

func runWithGauges(t *testing.T, scenario string, seed uint64, homes, gauges int, dur time.Duration) run {
	t.Helper()
	e, err := New(Config{Seed: seed, Start: testStart, Homes: homes, Segments: min(homes, 4), Scenario: Scenarios()[scenario], Duration: dur, RainGauges: gauges})
	if err != nil {
		t.Fatal(err)
	}
	col := &collectSink{}
	hs := NewHashSink()
	truth, err := e.Run(context.Background(), MultiSink{col, hs})
	if err != nil {
		t.Fatal(err)
	}
	return run{truth: truth, events: col.events, hash: hs.Sum(), engine: e}
}

func TestRainGauges(t *testing.T) {
	r := runWithGauges(t, "storm25", 7, 4, 2, 0)
	if len(r.truth.RainGauges) != 2 {
		t.Fatalf("truth has %d gauges", len(r.truth.RainGauges))
	}
	var trueMM float64
	for _, s := range r.truth.Rainfall {
		trueMM += s.Mm
	}
	byDev := map[string][]Event{}
	for _, ev := range r.events {
		if ev.Kind == DeviceKindRain {
			if ev.HomeIndex != -1 || ev.FPort != codec.PortRainGauge || ev.Confirmed || ev.DeviceName == "" {
				t.Fatalf("gauge event %+v", ev)
			}
			byDev[ev.DevEUI] = append(byDev[ev.DevEUI], ev)
		} else if ev.FPort == codec.PortRainGauge {
			t.Fatalf("house event on fPort 5: %+v", ev)
		}
	}
	for _, g := range r.truth.RainGauges {
		evs := byDev[g.DevEUI]
		if len(evs) != g.Uplinks || len(evs) == 0 {
			t.Fatalf("gauge %d: %d events, truth %d uplinks", g.Index, len(evs), g.Uplinks)
		}
		// Quantisation leaves less than one tip in the bucket.
		if g.TotalMm > trueMM+1e-9 || trueMM-g.TotalMm >= 0.2 {
			t.Errorf("gauge %d counted %.2f mm, true rain %.3f mm", g.Index, g.TotalMm, trueMM)
		}
		var prev *codec.RainGauge
		var prevAt time.Time
		for i, ev := range evs {
			u := decode(t, ev).(*codec.RainGauge)
			if ev.FCnt != uint32(i) {
				t.Fatalf("gauge %d uplink %d has fcnt %d", g.Index, i, ev.FCnt)
			}
			if i == 0 {
				if !u.Flags.Has(codec.RainCounterReset) || time.Duration(u.IntervalS)*time.Second != ev.Time.Sub(testStart) {
					t.Errorf("gauge %d first uplink %+v at %v", g.Index, u, ev.Time)
				}
			} else {
				gap := ev.Time.Sub(prevAt)
				if u.Flags != 0 || time.Duration(u.IntervalS)*time.Second != gap || u.TipCount < prev.TipCount {
					t.Errorf("gauge %d uplink %d: %+v after %v", g.Index, i, u, gap)
				}
				tipped := u.TipCount > prev.TipCount
				switch {
				case gap > 15*time.Minute:
					t.Errorf("gauge %d: %v without an uplink", g.Index, gap)
				case gap == 15*time.Minute:
				case !tipped || gap%(5*time.Minute) != 0:
					t.Errorf("gauge %d uplink %d after %v with tipped=%v", g.Index, i, gap, tipped)
				}
			}
			if u.MMPerTipUM != gaugeTipUM {
				t.Errorf("mm per tip %d", u.MMPerTipUM)
			}
			prev, prevAt = u, ev.Time
		}
		if last := prev; last.TipCount != g.Tips {
			t.Errorf("gauge %d last tip count %d, truth %d", g.Index, last.TipCount, g.Tips)
		}
	}
	if byDev["70b3d57ed1000000"] == nil || byDev["70b3d57ed1000001"] == nil {
		t.Fatalf("gauge dev_euis: %v", byDev)
	}
	// The ChirpStack event names the gauge and carries no segment tag.
	cs := byDev["70b3d57ed1000001"][0].ChirpStackEvent(DefaultIdentity(), "")
	if cs.GetDeviceInfo().GetDeviceName() != "sim-rain-02" || cs.GetDeviceInfo().GetTags()["kind"] != DeviceKindRain {
		t.Errorf("chirpstack device info = %v", cs.GetDeviceInfo())
	}
}

func TestRainGaugesDoNotPerturbHomes(t *testing.T) {
	a := runWithGauges(t, "storm25", 7, 4, 0, 12*time.Hour)
	b := runWithGauges(t, "storm25", 7, 4, 2, 12*time.Hour)
	c := runWithGauges(t, "storm25", 7, 4, 2, 12*time.Hour)
	if b.hash != c.hash {
		t.Fatal("gauges are not deterministic")
	}
	var homesOnly []Event
	for _, ev := range b.events {
		if ev.Kind == "" {
			homesOnly = append(homesOnly, ev)
		}
	}
	if len(homesOnly) != len(a.events) || len(homesOnly) == len(b.events) {
		t.Fatalf("%d house events with gauges, %d without, %d total", len(homesOnly), len(a.events), len(b.events))
	}
	for i := range homesOnly {
		x, y := a.events[i], homesOnly[i]
		x.Seq, y.Seq = 0, 0
		if x.DedupID != y.DedupID || string(x.Payload) != string(y.Payload) || !x.Time.Equal(y.Time) || x.DevEUI != y.DevEUI {
			t.Fatalf("house event %d differs with gauges: %+v vs %+v", i, x, y)
		}
	}
}

func TestTruthThresholdNotReachedIsMinusOne(t *testing.T) {
	// storm50 ends 12 h after the rain; slow homes cannot recede by then, and
	// the last minute of the run carries no rate sample (regression: it read 0).
	r := runScenario(t, "storm50", 42, 24, 8, 0)
	st := r.truth.Storms[0]
	endMin := r.engine.Duration().Minutes()
	notReached := 0
	for _, h := range st.Homes {
		if h.RecessionMin == -1 {
			notReached++
			continue
		}
		if at := st.RainEnd.Sub(testStart).Minutes() + h.RecessionMin; at >= endMin-1 || math.IsNaN(at) {
			t.Errorf("home %d recession %.0f min lands on the final minute", h.HomeIndex, h.RecessionMin)
		}
	}
	if notReached == 0 {
		t.Error("expected some homes not to recede within the scenario")
	}
}
