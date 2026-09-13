// Package seed loads the demo neighbourhood into the sumpnet database:
// illustrative street-segment polygons around Timberwalk, Ilderton
// (segments.geojson — not surveyed, not real lot lines), the simulator's
// homes and devices (so replayed telemetry lands on linked homes), and an
// optional owner link for one Clerk user. It is operator tooling (cmd/seed,
// `make seed`); no service uses it.
package seed

import (
	"context"
	"database/sql"
	_ "embed" // segments.geojson
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

//go:embed segments.geojson
var segmentsGeoJSON []byte

// Segment is one illustrative street segment.
type Segment struct {
	ID       string
	Name     string
	Kind     string
	Geometry json.RawMessage // GeoJSON geometry object
}

// Segments parses the embedded GeoJSON.
func Segments() ([]Segment, error) {
	var fc struct {
		Features []struct {
			Properties struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"properties"`
			Geometry json.RawMessage `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(segmentsGeoJSON, &fc); err != nil {
		return nil, fmt.Errorf("seed: segments.geojson: %w", err)
	}
	out := make([]Segment, 0, len(fc.Features))
	for _, f := range fc.Features {
		if f.Properties.ID == "" || len(f.Geometry) == 0 {
			return nil, errors.New("seed: segments.geojson: feature without id or geometry")
		}
		out = append(out, Segment{ID: f.Properties.ID, Name: f.Properties.Name, Kind: f.Properties.Kind, Geometry: f.Geometry})
	}
	return out, nil
}

// Options select the simulated neighbourhood to mirror. They must match the
// simulator run (`make sim SEED=… HOMES=…`), since home ids derive from the
// seed and devices from the home index.
type Options struct {
	Seed     uint64
	Homes    int
	Segments int
	// OwnerSubject (DEMO_OWNER_SUBJECT) is a Clerk user id to link to the
	// home at OwnerHome; empty links nobody.
	OwnerSubject string
	OwnerHome    int
}

// DefaultOptions mirror the simulator defaults.
func DefaultOptions() Options { return Options{Seed: 42, Homes: 60, Segments: 8} }

// Result summarises what Apply wrote.
type Result struct {
	Segments        int    `json:"segments"`
	SegmentsWithGeo int    `json:"segments_with_geometry"`
	Homes           int    `json:"homes"`
	Devices         int    `json:"devices"`
	OwnerHomeID     string `json:"owner_home_id,omitempty"`
	OwnerDevEUI     string `json:"owner_dev_eui,omitempty"`
}

// Apply upserts the demo neighbourhood through q (use a transaction). It is
// idempotent.
func Apply(ctx context.Context, q *sqlcgen.Queries, o Options) (Result, error) {
	if o.Homes <= 0 || o.Segments <= 0 || o.Segments > o.Homes {
		return Result{}, fmt.Errorf("seed: need 1 <= segments (%d) <= homes (%d)", o.Segments, o.Homes)
	}
	if o.OwnerSubject != "" && (o.OwnerHome < 0 || o.OwnerHome >= o.Homes) {
		return Result{}, fmt.Errorf("seed: owner home %d out of range", o.OwnerHome)
	}
	eng, err := sim.New(sim.Config{
		Seed: o.Seed, Start: time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		Homes: o.Homes, Segments: o.Segments, Scenario: sim.Scenarios()["dry-week"], Duration: time.Hour,
	})
	if err != nil {
		return Result{}, fmt.Errorf("seed: simulator: %w", err)
	}
	geo, err := Segments()
	if err != nil {
		return Result{}, err
	}
	byID := make(map[string]Segment, len(geo))
	for _, g := range geo {
		byID[g.ID] = g
	}

	var res Result
	for _, s := range eng.Segments() {
		p := sqlcgen.SeedSegmentParams{ID: s.ID, Name: s.Name, Kind: s.Kind.String()}
		if g, ok := byID[s.ID]; ok {
			if g.Kind != p.Kind {
				return Result{}, fmt.Errorf("seed: %s is %q in segments.geojson but %q in the simulator", s.ID, g.Kind, p.Kind)
			}
			p.Geometry = g.Geometry
			res.SegmentsWithGeo++
		}
		if err := q.SeedSegment(ctx, p); err != nil {
			return Result{}, fmt.Errorf("seed: segment %s: %w", s.ID, err)
		}
		res.Segments++
	}
	homes := eng.Homes()
	for _, h := range homes {
		id, err := uuid.Parse(h.HomeID)
		if err != nil {
			return Result{}, fmt.Errorf("seed: home %d id: %w", h.Index, err)
		}
		if _, err := q.UpsertHome(ctx, sqlcgen.UpsertHomeParams{
			ID: id, SegmentID: pgtype.Text{String: h.SegmentID, Valid: true},
			PitAreaM2: sql.NullFloat64{Float64: h.PitAreaM2, Valid: true},
		}); err != nil {
			return Result{}, fmt.Errorf("seed: home %d: %w", h.Index, err)
		}
		res.Homes++
		if _, err := q.UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{
			DevEui: h.DevEUI, Kind: "house", HomeID: uuid.NullUUID{UUID: id, Valid: true},
			Name: pgtype.Text{String: fmt.Sprintf("sim home %02d", h.Index), Valid: true},
		}); err != nil {
			return Result{}, fmt.Errorf("seed: device %s: %w", h.DevEUI, err)
		}
		res.Devices++
	}
	if o.OwnerSubject != "" {
		h := homes[o.OwnerHome]
		if _, err := q.SeedHomeOwner(ctx, sqlcgen.SeedHomeOwnerParams{AuthSubject: o.OwnerSubject, HomeID: uuid.MustParse(h.HomeID)}); err != nil {
			return Result{}, fmt.Errorf("seed: owner link: %w", err)
		}
		res.OwnerHomeID, res.OwnerDevEUI = h.HomeID, h.DevEUI
	}
	return res, nil
}
