package site

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"github.com/smhunt/sumpnet/internal/sim"
)

// Segment is one street, or one block of a long street, with its outline.
type Segment struct {
	ID     string
	Name   string
	Kind   sim.SegmentKind
	Street string
	Label  string
	Block  int // 1-based
	Blocks int
	Homes  int
	// HomesOutside counts homes whose point falls outside the outline (the
	// outline keeps only the segment's largest connected region).
	HomesOutside int
	LengthM      float64 // centreline length of the block
	// Geometry is a GeoJSON Polygon (lon/lat, counter-clockwise, closed).
	Geometry json.RawMessage

	ring []xy
}

// Home is one address of the site. Address, Lon and Lat never leave the
// process: sim.Site gets only the opaque identity and the segment.
type Home struct {
	Address  string
	Street   string
	Segment  int
	Lon, Lat float64
	Measure  float64 // metres along the street centreline

	HomeID  string
	DevEUI  string
	DevAddr string
	Stream  uint64
}

// Built is a site ready for the simulator and seed.
type Built struct {
	Name     string
	MunCode  string
	Segments []Segment
	// Homes are ordered by HomeID, a salted hash, so a home's index says
	// nothing about where on its street it is.
	Homes []Home
}

var homeIDNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/smhunt/sumpnet/site-home"))

// identity derives a home's identifiers from its address with an HMAC keyed
// by the private salt: stable for an address across sites and imports that
// share the salt, and unlinkable to the public address list without it.
// DevEUIs are locally administered (first octet 0x5e); streams have the top
// bit set as sim.SiteHome requires.
func identity(salt []byte, munCode, address string) (homeID, devEUI, devAddr string, stream uint64) {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte("sumpnet/site-home/v1\x00" + munCode + "\x00" + address))
	sum := mac.Sum(nil)
	homeID = uuid.NewSHA1(homeIDNamespace, sum).String()
	devEUI = "5e" + hex.EncodeToString(sum[0:7])
	devAddr = "02" + hex.EncodeToString(sum[7:10])
	stream = binary.BigEndian.Uint64(sum[10:18]) | 1<<63
	return homeID, devEUI, devAddr, stream
}

// Build turns a snapshot into segments and homes. It is deterministic: the
// same snapshot always gives the same segments, outlines and identities.
func Build(s *Snapshot) (*Built, error) {
	if s == nil || s.Version != SnapshotVersion {
		return nil, fmt.Errorf("site: need a version %d snapshot", SnapshotVersion)
	}
	cfg := s.Config
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	salt, err := hex.DecodeString(s.IdentitySalt)
	if err != nil || len(salt) < 16 {
		return nil, fmt.Errorf("site: snapshot %q has no usable identity salt", s.Site)
	}
	var sumLon, sumLat float64
	n := 0
	for _, st := range s.Streets {
		for _, a := range st.Addresses {
			sumLon, sumLat, n = sumLon+a.Lon, sumLat+a.Lat, n+1
		}
	}
	if n == 0 {
		return nil, fmt.Errorf("site: snapshot %q has no addresses", s.Site)
	}
	pr := newProjection(sumLon/float64(n), sumLat/float64(n))

	b := &Built{Name: s.Site, MunCode: cfg.MunCode}
	var caps []capsule
	for _, st := range s.Streets {
		kind, kerr := ParseKind(st.Kind)
		if kerr != nil {
			return nil, fmt.Errorf("site: %s: %w", st.Name, kerr)
		}
		var chains [][]xy
		for _, ch := range st.Centreline {
			var line []xy
			for _, v := range ch {
				line = append(line, pr.fwd(v[0], v[1]))
			}
			chains = append(chains, line)
		}
		cl := newCentreline(chains)
		if len(st.Addresses) == 0 || cl.length <= 0 {
			return nil, fmt.Errorf("site: %s needs addresses (%d) and a centreline (%.0f m)", st.Name, len(st.Addresses), cl.length)
		}
		type located struct {
			a       Address
			p, foot xy
			m       float64
		}
		locs := make([]located, 0, len(st.Addresses))
		for _, a := range st.Addresses {
			p := pr.fwd(a.Lon, a.Lat)
			m, foot, _ := cl.locate(p)
			locs = append(locs, located{a: a, p: p, foot: foot, m: m})
		}
		sort.SliceStable(locs, func(i, j int) bool {
			if locs[i].m != locs[j].m {
				return locs[i].m < locs[j].m
			}
			return locs[i].a.Key < locs[j].a.Key
		})
		blocks := (len(locs) + cfg.MaxHomesPerSegment - 1) / cfg.MaxHomesPerSegment
		start := 0
		for blk := 0; blk < blocks; blk++ {
			size := len(locs) / blocks
			if blk < len(locs)%blocks {
				size++
			}
			group := locs[start : start+size]
			lo, hi := 0.0, cl.length
			if blk > 0 {
				lo = (locs[start-1].m + group[0].m) / 2
			}
			if blk < blocks-1 {
				hi = (group[len(group)-1].m + locs[start+size].m) / 2
			}
			segIdx := len(b.Segments)
			name := st.Label
			if blocks > 1 {
				name = fmt.Sprintf("%s (part %d of %d)", st.Label, blk+1, blocks)
			}
			b.Segments = append(b.Segments, Segment{
				ID: slug(st.Name) + "-" + strconv.Itoa(blk+1), Name: name, Kind: kind, Street: st.Name, Label: st.Label,
				Block: blk + 1, Blocks: blocks, Homes: len(group), LengthM: math.Round((hi-lo)*10) / 10,
			})
			for _, part := range cl.slice(lo, hi) {
				if len(part) == 1 {
					caps = append(caps, capsule{a: part[0], b: part[0], label: segIdx})
				}
				for k := 1; k < len(part); k++ {
					caps = append(caps, capsule{a: part[k-1], b: part[k], label: segIdx})
				}
			}
			for _, g := range group {
				caps = append(caps, capsule{a: g.p, b: g.foot, label: segIdx})
				hid, eui, addr, stream := identity(salt, cfg.MunCode, g.a.Key)
				b.Homes = append(b.Homes, Home{
					Address: g.a.Key, Street: st.Name, Segment: segIdx, Lon: g.a.Lon, Lat: g.a.Lat, Measure: g.m,
					HomeID: hid, DevEUI: eui, DevAddr: addr, Stream: stream,
				})
			}
			start += size
		}
	}

	rings, err := outlines(caps, len(b.Segments), defaultOutline)
	if err != nil {
		return nil, fmt.Errorf("site: %s outlines: %w", s.Site, err)
	}
	for i := range b.Segments {
		if rings[i] == nil {
			return nil, fmt.Errorf("site: segment %s has no outline", b.Segments[i].ID)
		}
		geo, err := polygonGeoJSON(rings[i], pr)
		if err != nil {
			return nil, fmt.Errorf("site: segment %s: %w", b.Segments[i].ID, err)
		}
		b.Segments[i].Geometry, b.Segments[i].ring = geo, rings[i]
	}
	for _, h := range b.Homes {
		if seg := &b.Segments[h.Segment]; !pointInRing(pr.fwd(h.Lon, h.Lat), seg.ring) {
			seg.HomesOutside++
		}
	}

	sort.SliceStable(b.Homes, func(i, j int) bool { return b.Homes[i].HomeID < b.Homes[j].HomeID })
	seen := make(map[string]bool, 3*len(b.Homes))
	for _, h := range b.Homes {
		stream := "s" + strconv.FormatUint(h.Stream, 16)
		if seen["h"+h.HomeID] || seen["d"+h.DevEUI] || seen[stream] {
			return nil, fmt.Errorf("site: identity collision in %s; import with a new salt", s.Site)
		}
		seen["h"+h.HomeID], seen["d"+h.DevEUI], seen[stream] = true, true, true
	}
	return b, nil
}

// polygonGeoJSON converts a counter-clockwise ring in metres to a closed
// GeoJSON Polygon, rounding to 1e-7 degrees (≈1 cm).
func polygonGeoJSON(ring []xy, pr projection) (json.RawMessage, error) {
	ll := make([]xy, len(ring))
	for k, p := range ring {
		lon, lat := pr.inv(p)
		ll[k] = xy{math.Round(lon*1e7) / 1e7, math.Round(lat*1e7) / 1e7}
	}
	if !ringSimple(ll) {
		return nil, fmt.Errorf("%w: ring is not simple after rounding", errOutline)
	}
	if signedArea(ll) < 0 {
		for i, j := 0, len(ll)-1; i < j; i, j = i+1, j-1 {
			ll[i], ll[j] = ll[j], ll[i]
		}
	}
	coords := make([][2]float64, 0, len(ll)+1)
	for _, p := range ll {
		coords = append(coords, [2]float64{p.X, p.Y})
	}
	coords = append(coords, coords[0])
	return json.Marshal(struct {
		Type        string         `json:"type"`
		Coordinates [][][2]float64 `json:"coordinates"`
	}{"Polygon", [][][2]float64{coords}})
}

// SimSite is the site as the simulator sees it: no addresses.
func (b *Built) SimSite() *sim.Site {
	s := &sim.Site{Name: b.Name}
	for _, seg := range b.Segments {
		s.Segments = append(s.Segments, sim.SiteSegment{ID: seg.ID, Name: seg.Name, Kind: seg.Kind})
	}
	for _, h := range b.Homes {
		s.Homes = append(s.Homes, sim.SiteHome{HomeID: h.HomeID, DevEUI: h.DevEUI, DevAddr: h.DevAddr, Stream: h.Stream, Segment: h.Segment, Lon: h.Lon, Lat: h.Lat})
	}
	return s
}

// Locate returns the index of the home at address (NormaliseAddress form,
// e.g. "<number> <STREET>" or "<number> <STREET> UNIT <unit>").
func (b *Built) Locate(address string) (int, bool) {
	key := NormaliseAddress(address)
	for i, h := range b.Homes {
		if h.Address == key {
			return i, true
		}
	}
	return -1, false
}

// SegmentsGeoJSON is a FeatureCollection of the segment outlines with their
// ids, names, kinds and home counts (no addresses or house positions), for
// checking an import in a GIS tool.
func (b *Built) SegmentsGeoJSON() ([]byte, error) {
	type props struct {
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		Kind    string  `json:"kind"`
		Block   int     `json:"block"`
		Blocks  int     `json:"blocks"`
		Homes   int     `json:"homes"`
		LengthM float64 `json:"length_m"`
	}
	type feature struct {
		Type       string          `json:"type"`
		Properties props           `json:"properties"`
		Geometry   json.RawMessage `json:"geometry"`
	}
	fc := struct {
		Type        string    `json:"type"`
		Name        string    `json:"name"`
		Attribution string    `json:"attribution"`
		LicenceNote string    `json:"licence_note"`
		Features    []feature `json:"features"`
	}{Type: "FeatureCollection", Name: "sumpnet-site-" + b.Name, Attribution: Attribution, LicenceNote: LicenceNote}
	for _, s := range b.Segments {
		fc.Features = append(fc.Features, feature{Type: "Feature", Geometry: s.Geometry, Properties: props{
			ID: s.ID, Name: s.Name, Kind: s.Kind.String(), Block: s.Block, Blocks: s.Blocks, Homes: s.Homes, LengthM: s.LengthM,
		}})
	}
	return json.MarshalIndent(fc, "", " ")
}
