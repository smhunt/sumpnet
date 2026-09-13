package site

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SnapshotVersion is the snapshot file format version.
const SnapshotVersion = 1

// PrivacyNote is written into every snapshot.
const PrivacyNote = "Contains real addresses and per-house coordinates. Gitignored; never commit it, and never copy addresses, coordinates or the identity salt into the database, protos, REST, the dashboard, tests, docs or commit messages (ADR 0005, ADR 0008)."

// StreetSnapshot is one street's normalised data.
type StreetSnapshot struct {
	Name          string       `json:"name"`
	RoadName      string       `json:"road_name"`
	Label         string       `json:"label"`
	Kind          string       `json:"kind"`
	AddressSource SourceRef    `json:"address_source"`
	RoadSource    SourceRef    `json:"road_source"`
	AddressStats  AddressStats `json:"address_stats"`
	RoadStats     RoadStats    `json:"road_stats"`
	Addresses     []Address    `json:"addresses"`
	RoadPieces    []RoadPiece  `json:"road_pieces"`
	// Centreline is the road pieces joined into chains (lon, lat).
	Centreline [][][2]float64 `json:"centreline"`
}

// Snapshot is a site's normalised County data: the gitignored file
// data/sites/<site>.json that the simulator and seed read.
type Snapshot struct {
	Version     int       `json:"version"`
	Site        string    `json:"site"`
	Description string    `json:"description"`
	GeneratedAt time.Time `json:"generated_at"`
	Attribution string    `json:"attribution"`
	LicenceNote string    `json:"licence_note"`
	PrivacyNote string    `json:"privacy_note"`
	// Config is the resolved site config the snapshot was built from.
	Config Config `json:"config"`
	// IdentitySalt (hex) keys the home identifiers; it is copied from the
	// cache so a snapshot alone reproduces the same ids.
	IdentitySalt string           `json:"identity_salt"`
	Streets      []StreetSnapshot `json:"streets"`
}

// Importer builds snapshots from a Source.
type Importer struct {
	Source *Source
	Salt   []byte
	Now    func() time.Time
}

// chainTolM joins centreline pieces whose endpoints are this close.
const chainTolM = 1.0

// Import reads (or fetches) every street of cfg and normalises it.
func (im *Importer) Import(ctx context.Context, cfg Config) (*Snapshot, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(im.Salt) < 16 {
		return nil, errors.New("site: identity salt must be at least 16 bytes")
	}
	now := time.Now
	if im.Now != nil {
		now = im.Now
	}
	s := &Snapshot{
		Version: SnapshotVersion, Site: cfg.Name, Description: cfg.Description, GeneratedAt: now().UTC().Truncate(time.Second),
		Attribution: Attribution, LicenceNote: LicenceNote, PrivacyNote: PrivacyNote, Config: cfg, IdentitySalt: fmt.Sprintf("%x", im.Salt),
	}
	for _, sc := range cfg.Streets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st := StreetSnapshot{Name: sc.Name, RoadName: sc.Road(), Label: sc.Label, Kind: sc.Kind}
		acf, aref, err := im.Source.Addresses(ctx, sc.Name, cfg.MunCode)
		if err != nil {
			return nil, err
		}
		st.AddressSource = aref
		if st.Addresses, st.AddressStats, err = normaliseAddresses(acf.Pages, sc.Name, cfg.MunCode); err != nil {
			return nil, fmt.Errorf("site: %s addresses: %w", sc.Name, err)
		}
		if len(st.Addresses) == 0 {
			return nil, fmt.Errorf("site: %s has no address points in %s (check the street name)", sc.Name, cfg.MunCode)
		}
		rcf, rref, err := im.Source.Roads(ctx, sc.Road(), cfg.MunCode)
		if err != nil {
			return nil, err
		}
		st.RoadSource = rref
		if st.RoadPieces, st.RoadStats, err = normaliseRoads(rcf.Pages, sc.Road(), cfg.MunCode); err != nil {
			return nil, fmt.Errorf("site: %s roads: %w", sc.Road(), err)
		}
		if len(st.RoadPieces) == 0 {
			return nil, fmt.Errorf("site: road %s has no centreline in %s (set road_name if the road network spells it differently)", sc.Road(), cfg.MunCode)
		}
		st.Centreline = joinPieces(st.RoadPieces)
		s.Streets = append(s.Streets, st)
	}
	return s, nil
}

// joinPieces chains road pieces, keeping the source coordinates exactly.
func joinPieces(pieces []RoadPiece) [][][2]float64 {
	p0 := pieces[0].Path[0]
	pr := newProjection(p0[0], p0[1])
	proj := make([][]xy, len(pieces))
	for i, rp := range pieces {
		for _, v := range rp.Path {
			proj[i] = append(proj[i], pr.fwd(v[0], v[1]))
		}
	}
	var out [][][2]float64
	for _, chain := range chainOrder(proj, chainTolM) {
		var line [][2]float64
		for _, ref := range chain {
			path := pieces[ref.index].Path
			for k := range path {
				v := path[k]
				if ref.reversed {
					v = path[len(path)-1-k]
				}
				if k == 0 && len(line) > 0 {
					continue // the joint is the previous piece's last vertex
				}
				line = append(line, v)
			}
		}
		out = append(out, line)
	}
	return out
}

// Write saves the snapshot as indented JSON.
func (s *Snapshot) Write(path string) error { return writeJSON(path, s, true) }

// LoadSnapshot reads a snapshot written by Write.
func LoadSnapshot(path string) (*Snapshot, error) {
	var s Snapshot
	if err := readJSON(path, &s); err != nil {
		return nil, fmt.Errorf("site: load snapshot (run make site-import first): %w", err)
	}
	if s.Version != SnapshotVersion {
		return nil, fmt.Errorf("site: %s is snapshot version %d, want %d; re-run make site-import", path, s.Version, SnapshotVersion)
	}
	return &s, nil
}
