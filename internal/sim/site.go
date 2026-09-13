package sim

import (
	"errors"
	"fmt"
	"math"
)

// Site is a real-geography neighbourhood: its segments are real streets (or
// blocks of a long street) and its homes are real address points. internal/site
// builds it from the County of Middlesex snapshot.
//
// A Site carries no address text. Each home is identified by opaque values
// derived from a salted hash of its address, so nothing the simulator emits
// (events, truth, device names) can be traced back to an address without the
// gitignored snapshot. Homes should be ordered by those opaque values, not by
// position along the street, so a home's index says nothing about where it is.
type Site struct {
	Name     string
	Segments []SiteSegment
	Homes    []SiteHome
}

// SiteSegment is one street segment of a Site.
type SiteSegment struct {
	ID   string
	Name string
	Kind SegmentKind
}

// SiteHome is one home of a Site.
type SiteHome struct {
	HomeID  string // UUID
	DevEUI  string // 16 lowercase hex
	DevAddr string // 8 lowercase hex
	// Stream is the home's PCG stream; it must have the top bit set so it can
	// never collide with the scenario stream (0), synthetic homes (1..N) or
	// rain gauges (1<<40 + i). Deriving it from the address keeps a home's
	// parameters stable when streets are added to or removed from a site.
	Stream  uint64
	Segment int // index into Site.Segments
	// Lon, Lat are WGS84 degrees. The simulator only uses them to place the
	// rain gauges at the ends of the site; they are never emitted.
	Lon, Lat float64
}

const siteStreamBit = 1 << 63

func (s *Site) validate() error {
	if s == nil {
		return errors.New("sim: nil site")
	}
	if len(s.Segments) == 0 || len(s.Homes) == 0 {
		return fmt.Errorf("sim: site %q needs segments (%d) and homes (%d)", s.Name, len(s.Segments), len(s.Homes))
	}
	segIDs := make(map[string]bool, len(s.Segments))
	for i, seg := range s.Segments {
		if seg.ID == "" || segIDs[seg.ID] {
			return fmt.Errorf("sim: site %q segment %d: empty or duplicate id %q", s.Name, i, seg.ID)
		}
		if _, ok := kindMultipliers[seg.Kind]; !ok {
			return fmt.Errorf("sim: site %q segment %s: unknown kind %d", s.Name, seg.ID, seg.Kind)
		}
		segIDs[seg.ID] = true
	}
	homes := make([]int, len(s.Segments))
	ids := make(map[string]bool, 2*len(s.Homes))
	streams := make(map[uint64]bool, len(s.Homes))
	for i, h := range s.Homes {
		if h.Segment < 0 || h.Segment >= len(s.Segments) {
			return fmt.Errorf("sim: site %q home %d: segment %d out of range", s.Name, i, h.Segment)
		}
		if h.HomeID == "" || h.DevEUI == "" || h.DevAddr == "" || ids["h"+h.HomeID] || ids["d"+h.DevEUI] {
			return fmt.Errorf("sim: site %q home %d: empty or duplicate identity", s.Name, i)
		}
		if h.Stream&siteStreamBit == 0 || streams[h.Stream] {
			return fmt.Errorf("sim: site %q home %d: stream must be unique with the top bit set", s.Name, i)
		}
		if math.IsNaN(h.Lon) || math.IsNaN(h.Lat) {
			return fmt.Errorf("sim: site %q home %d: bad coordinates", s.Name, i)
		}
		ids["h"+h.HomeID], ids["d"+h.DevEUI], streams[h.Stream] = true, true, true
		homes[h.Segment]++
	}
	for i, n := range homes {
		if n == 0 {
			return fmt.Errorf("sim: site %q segment %s has no homes", s.Name, s.Segments[i].ID)
		}
	}
	return nil
}

// siteSegments builds SegmentParams for a site, in site order.
func siteSegments(s *Site) []SegmentParams {
	segs := make([]SegmentParams, len(s.Segments))
	for i, ss := range s.Segments {
		m := kindMultipliers[ss.Kind]
		segs[i] = SegmentParams{
			Index: i, ID: ss.ID, Name: ss.Name, Kind: ss.Kind,
			ResponseMul: m.response, BaseflowMul: m.baseflow, ReservoirMul: m.reservoir,
		}
	}
	for i, h := range s.Homes {
		segs[h.Segment].HomeIndexes = append(segs[h.Segment].HomeIndexes, i)
	}
	return segs
}

// siteGaugePlacement puts gauge i at an end of the site's extent along its
// longer axis (north/south or west/east), on the extent's centre line.
func siteGaugePlacement(s *Site, i int) (loc string, lon, lat float64) {
	west, east, south, north := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for _, h := range s.Homes {
		west, east = math.Min(west, h.Lon), math.Max(east, h.Lon)
		south, north = math.Min(south, h.Lat), math.Max(north, h.Lat)
	}
	midLon, midLat := (west+east)/2, (south+north)/2
	// Compare spans in metres (a degree of longitude is shorter by cos(lat)).
	northSouth := (north - south) >= (east-west)*math.Cos(midLat*math.Pi/180)
	switch {
	case i == 0 && northSouth:
		loc, lon, lat = "north end", midLon, north
	case i == 1 && northSouth:
		loc, lon, lat = "south end", midLon, south
	case i == 0:
		loc, lon, lat = "west end", west, midLat
	case i == 1:
		loc, lon, lat = "east end", east, midLat
	default:
		loc, lon, lat = fmt.Sprintf("gauge %d (centre)", i+1), midLon, midLat
	}
	lon, lat = math.Round(lon*1e4)/1e4, math.Round(lat*1e4)/1e4
	return fmt.Sprintf("%s of %s (%.4f, %.4f)", loc, s.Name, lat, lon), lon, lat
}
