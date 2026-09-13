package seed

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/sim"
)

func TestSegmentsMatchSimulator(t *testing.T) {
	segs, err := Segments()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := sim.New(sim.Config{Seed: 42, Start: time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), Homes: 60, Segments: 8, Scenario: sim.Scenarios()["dry-week"], Duration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != len(eng.Segments()) {
		t.Fatalf("geojson has %d segments, simulator %d", len(segs), len(eng.Segments()))
	}
	for i, s := range eng.Segments() {
		g := segs[i]
		if g.ID != s.ID || g.Kind != s.Kind.String() || g.Name != s.Name {
			t.Errorf("feature %d = %s/%s/%q, simulator %s/%s/%q", i, g.ID, g.Kind, g.Name, s.ID, s.Kind, s.Name)
		}
		var poly struct {
			Type        string         `json:"type"`
			Coordinates [][][2]float64 `json:"coordinates"`
		}
		if err := json.Unmarshal(g.Geometry, &poly); err != nil || poly.Type != "Polygon" || len(poly.Coordinates) != 1 {
			t.Fatalf("%s geometry: %v %+v", g.ID, err, poly)
		}
		ring := poly.Coordinates[0]
		if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
			t.Errorf("%s ring is not closed", g.ID)
		}
		for _, p := range ring { // near Ilderton, Middlesex Centre, ON
			if p[0] < -81.45 || p[0] > -81.40 || p[1] < 43.04 || p[1] > 43.07 {
				t.Errorf("%s point %v is outside the Ilderton area", g.ID, p)
			}
		}
	}
}

func TestApplyValidatesOptions(t *testing.T) {
	tests := []Options{
		{Seed: 1, Homes: 0, Segments: 1},
		{Seed: 1, Homes: 4, Segments: 5},
		{Seed: 1, Homes: 4, Segments: 2, OwnerSubject: "user_x", OwnerHome: 4},
	}
	for _, o := range tests {
		if _, err := Apply(t.Context(), nil, o); err == nil {
			t.Errorf("Apply(%+v) accepted invalid options", o)
		}
	}
}
