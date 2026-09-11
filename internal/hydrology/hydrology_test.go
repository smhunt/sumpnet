package hydrology

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC)

func cyc(start time.Duration, runS, startMM, endMM int32) Cycle {
	return Cycle{StartedAt: t0.Add(start), RunS: runS, LevelStartMM: startMM, LevelEndMM: endMM, Pump: "primary"}
}

func TestClassification(t *testing.T) {
	cases := []struct {
		name                   string
		c                      Cycle
		cycle, dry, continuous bool
	}{
		{"normal cycle", cyc(0, 18, 420, 610), true, false, false},
		{"run just at 3 s and drop 20", cyc(0, 3, 400, 420), true, false, false},
		{"run 2 s", cyc(0, 2, 400, 500), false, false, false},
		{"drop 19 mm", cyc(0, 10, 400, 419), false, false, false},
		{"dry run: 30 s, 4 mm", cyc(0, 30, 400, 404), false, true, false},
		{"dry run boundary drop 5 mm is not dry", cyc(0, 30, 400, 405), false, false, false},
		{"dry run boundary 29 s", cyc(0, 29, 400, 401), false, false, false},
		{"water rose during run", cyc(0, 45, 400, 380), false, true, false},
		{"continuous 601 s", cyc(0, 601, 400, 700), true, false, true},
		{"continuous boundary 600 s", cyc(0, 600, 400, 700), true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCycle(tc.c); got != tc.cycle {
				t.Errorf("IsCycle = %v, want %v", got, tc.cycle)
			}
			if got := IsDryRun(tc.c); got != tc.dry {
				t.Errorf("IsDryRun = %v, want %v", got, tc.dry)
			}
			if got := IsContinuousRun(tc.c); got != tc.continuous {
				t.Errorf("IsContinuousRun = %v, want %v", got, tc.continuous)
			}
		})
	}
}

func TestShortCyclingRun(t *testing.T) {
	// Each cycle runs 10 s; gaps are measured from the previous cycle's end.
	seq := func(gaps ...time.Duration) []Cycle {
		var out []Cycle
		at := time.Duration(0)
		for i, g := range gaps {
			if i > 0 {
				at += 10*time.Second + g
			}
			out = append(out, cyc(at, 10, 420, 610))
		}
		return out
	}
	cases := []struct {
		name string
		in   []Cycle
		want int
	}{
		{"empty", nil, 0},
		{"single", seq(0), 1},
		{"four close cycles", seq(0, 30*time.Second, 30*time.Second, 30*time.Second), 4},
		{"five close cycles", seq(0, 30*time.Second, 30*time.Second, 30*time.Second, 30*time.Second), 5},
		{"gap of exactly 60 s breaks the run", seq(0, 30*time.Second, 60*time.Second, 30*time.Second, 30*time.Second), 3},
		{"gap of 59 s keeps the run", seq(0, 30*time.Second, 59*time.Second, 30*time.Second, 30*time.Second), 5},
		{"only the trailing run counts", seq(0, 5*time.Second, 5*time.Second, 5*time.Second, time.Hour, 5*time.Second), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShortCyclingRun(tc.in); got != tc.want {
				t.Errorf("ShortCyclingRun = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEstVolume(t *testing.T) {
	// 18" basin (0.164 m²) with a 150 mm hysteresis pumps 24.6 L per cycle.
	if got := EstVolumeL(0.164, cyc(0, 18, 350, 500)); math.Abs(got-24.6) > 1e-9 {
		t.Errorf("EstVolumeL = %v, want 24.6", got)
	}
	if got := EstVolumeL(0.164, cyc(0, 45, 400, 380)); got >= 0 {
		t.Errorf("rising water must give a negative volume, got %v", got)
	}
}

func TestLevelRising(t *testing.T) {
	cases := []struct {
		name string
		in   []int32
		min  int32
		want bool
	}{
		{"one sample", []int32{400}, 10, false},
		{"rising 3 samples", []int32{420, 410, 395}, 10, true},
		{"rising but under the minimum", []int32{420, 417, 414}, 10, false},
		{"noise reversal", []int32{420, 410, 412}, 10, false},
		{"flat step", []int32{420, 410, 410}, 5, false},
		{"falling", []int32{380, 400, 420}, 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LevelRising(tc.in, tc.min); got != tc.want {
				t.Errorf("LevelRising = %v, want %v", got, tc.want)
			}
		})
	}
}
