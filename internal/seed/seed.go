// Package seed loads the demo neighbourhood into the sumpnet database:
// street segments with polygons, the simulator's homes and devices (so
// replayed telemetry lands on linked homes), and an optional owner link for
// one Clerk user. Without a site the segments are illustrative polygons near
// Ilderton (segments.geojson — not surveyed, not real streets); with a site
// (internal/site) they are Timberwalk's real streets and one home per County
// address point. It is operator tooling (cmd/seed, `make seed`); no service
// uses it.
//
// Privacy (ADR 0005, ADR 0008): nothing written carries an address or a
// house position. Homes get salted ids, devices a neutral name, segments
// their street outline; the address is used only in memory to find the
// owner's home.
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
	"github.com/smhunt/sumpnet/internal/site"
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
	// home at OwnerHome (or, with a site, at OwnerAddress); empty links nobody.
	OwnerSubject string
	OwnerHome    int
	// Site, when set, replaces the synthetic neighbourhood (Homes and
	// Segments are ignored); match `make sim SITE=…`. Home ids come from the
	// site, so only the pit areas depend on Seed.
	Site *site.Built
	// OwnerAddress (OWNER_ADDRESS, "<number> <STREET>") picks the owner's home
	// in Site. It is required with a site and an owner: a real home is never
	// linked by index.
	OwnerAddress string
}

// DefaultOptions mirror the simulator defaults.
func DefaultOptions() Options { return Options{Seed: 42, Homes: 60, Segments: 8} }

// Result summarises what Apply wrote.
type Result struct {
	Site            string `json:"site,omitempty"`
	Segments        int    `json:"segments"`
	SegmentsWithGeo int    `json:"segments_with_geometry"`
	Homes           int    `json:"homes"`
	Devices         int    `json:"devices"`
	OwnerHomeID     string `json:"owner_home_id,omitempty"`
	OwnerDevEUI     string `json:"owner_dev_eui,omitempty"`
	OwnerSegment    string `json:"owner_segment,omitempty"`
}

// Apply upserts the demo neighbourhood through q (use a transaction). It is
// idempotent.
func Apply(ctx context.Context, q *sqlcgen.Queries, o Options) (Result, error) {
	cfg := sim.Config{Seed: o.Seed, Start: time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), Scenario: sim.Scenarios()["dry-week"], Duration: time.Hour}
	var geo []Segment
	ownerHome := o.OwnerHome
	var res Result
	if o.Site != nil {
		cfg.Site = o.Site.SimSite()
		res.Site = o.Site.Name
		for _, s := range o.Site.Segments {
			geo = append(geo, Segment{ID: s.ID, Name: s.Name, Kind: s.Kind.String(), Geometry: s.Geometry})
		}
		if o.OwnerSubject != "" {
			if o.OwnerAddress == "" {
				return Result{}, fmt.Errorf("seed: site %s: set OWNER_ADDRESS to link %s (a real home is never linked by index)", o.Site.Name, o.OwnerSubject)
			}
			i, ok := o.Site.Locate(o.OwnerAddress)
			if !ok {
				return Result{}, fmt.Errorf("seed: OWNER_ADDRESS is not an address in site %s", o.Site.Name)
			}
			ownerHome = i
		}
	} else {
		if o.Homes <= 0 || o.Segments <= 0 || o.Segments > o.Homes {
			return Result{}, fmt.Errorf("seed: need 1 <= segments (%d) <= homes (%d)", o.Segments, o.Homes)
		}
		if o.OwnerSubject != "" && (o.OwnerHome < 0 || o.OwnerHome >= o.Homes) {
			return Result{}, fmt.Errorf("seed: owner home %d out of range", o.OwnerHome)
		}
		if o.OwnerAddress != "" {
			return Result{}, errors.New("seed: OWNER_ADDRESS needs a site (SITE=…)")
		}
		cfg.Homes, cfg.Segments = o.Homes, o.Segments
		var err error
		if geo, err = Segments(); err != nil {
			return Result{}, err
		}
	}
	eng, err := sim.New(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("seed: simulator: %w", err)
	}
	byID := make(map[string]Segment, len(geo))
	for _, g := range geo {
		byID[g.ID] = g
	}

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
		// Site homes are ordered by their salted id, so the index names no position.
		name := fmt.Sprintf("sim home %02d", h.Index)
		if o.Site != nil {
			name = fmt.Sprintf("sim %s home %03d", o.Site.Name, h.Index)
		}
		if _, err := q.UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{
			DevEui: h.DevEUI, Kind: "house", HomeID: uuid.NullUUID{UUID: id, Valid: true},
			Name: pgtype.Text{String: name, Valid: true},
		}); err != nil {
			return Result{}, fmt.Errorf("seed: device %s: %w", h.DevEUI, err)
		}
		res.Devices++
	}
	if o.OwnerSubject != "" {
		h := homes[ownerHome]
		if _, err := q.SeedHomeOwner(ctx, sqlcgen.SeedHomeOwnerParams{AuthSubject: o.OwnerSubject, HomeID: uuid.MustParse(h.HomeID)}); err != nil {
			return Result{}, fmt.Errorf("seed: owner link: %w", err)
		}
		res.OwnerHomeID, res.OwnerDevEUI = h.HomeID, h.DevEUI
		if o.Site != nil {
			res.OwnerSegment = h.SegmentID
		}
	}
	return res, nil
}
