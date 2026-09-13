package site

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Address is one normalised address point (snapshot only; never leaves it).
type Address struct {
	// Key is the civic address, e.g. "12 EXAMPLE CRES" or "1 EXAMPLE LANE UNIT 4".
	Key      string  `json:"key"`
	Number   string  `json:"number"`
	Unit     string  `json:"unit,omitempty"`
	ObjectID int64   `json:"object_id"`
	Lon      float64 `json:"lon"`
	Lat      float64 `json:"lat"`
}

// AddressStats counts what normalisation kept and dropped for one street.
type AddressStats struct {
	Raw               int `json:"raw"`
	Kept              int `json:"kept"`
	OtherStreet       int `json:"other_street"`
	NoGeometry        int `json:"no_geometry"`
	NoNumber          int `json:"no_number"`
	DuplicateGlobalID int `json:"duplicate_global_id"`
	DuplicateAddress  int `json:"duplicate_address"`
	// ParentOfUnits counts civic points dropped because unit points share
	// their number (a townhouse block: the units are the dwellings).
	ParentOfUnits int `json:"parent_of_units"`
}

// RoadPiece is one centreline path of the road network, in WGS84.
type RoadPiece struct {
	ObjectID int64        `json:"object_id"`
	Path     [][2]float64 `json:"path"`
}

// RoadStats counts what normalisation kept and dropped for one road.
type RoadStats struct {
	Features          int `json:"features"`
	Pieces            int `json:"pieces"`
	OtherRoad         int `json:"other_road"`
	OtherMunicipality int `json:"other_municipality"`
	Proposed          int `json:"proposed"`
	Degenerate        int `json:"degenerate"`
}

type rawPage struct {
	SpatialReference *struct {
		WKID       int `json:"wkid"`
		LatestWKID int `json:"latestWkid"`
	} `json:"spatialReference"`
	Features []rawFeature `json:"features"`
}

type rawFeature struct {
	Attributes map[string]any `json:"attributes"`
	Geometry   *struct {
		X     *float64      `json:"x"`
		Y     *float64      `json:"y"`
		Paths [][][]float64 `json:"paths"`
	} `json:"geometry"`
}

// decodePages checks every non-empty page is in WGS84 (outSR 4326) and
// returns the features in page order.
func decodePages(pages []json.RawMessage) ([]rawFeature, error) {
	var out []rawFeature
	for i, p := range pages {
		var rp rawPage
		if err := json.Unmarshal(p, &rp); err != nil {
			return nil, fmt.Errorf("site: page %d: %w", i, err)
		}
		if len(rp.Features) == 0 {
			continue
		}
		if rp.SpatialReference == nil || (rp.SpatialReference.WKID != 4326 && rp.SpatialReference.LatestWKID != 4326) {
			return nil, fmt.Errorf("site: page %d is not in WGS84 (EPSG:4326): %+v", i, rp.SpatialReference)
		}
		out = append(out, rp.Features...)
	}
	return out, nil
}

func attrString(a map[string]any, k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func attrInt(a map[string]any, k string) (int64, bool) {
	switch v := a[k].(type) {
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int64(v), true
		}
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func validLonLat(lon, lat float64) bool {
	return !math.IsNaN(lon) && !math.IsNaN(lat) && lon >= -180 && lon <= 180 && lat >= -90 && lat <= 90 && (lon != 0 || lat != 0)
}

var civicRE = regexp.MustCompile(`^[0-9]+[A-Z]?$`)

// NormaliseAddress upper-cases an address and collapses its whitespace:
// " 12  example cres " becomes "12 EXAMPLE CRES".
func NormaliseAddress(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), " "))
}

// normaliseAddresses turns one street's raw address pages into unique
// dwellings, sorted by civic number and unit. Points are deduplicated by
// GlobalID, then by address (the lowest OBJECTID_1 wins).
func normaliseAddresses(pages []json.RawMessage, street, munCode string) ([]Address, AddressStats, error) {
	var st AddressStats
	feats, err := decodePages(pages)
	if err != nil {
		return nil, st, err
	}
	st.Raw = len(feats)
	type cand struct {
		a   Address
		gid string
	}
	var cands []cand
	for _, f := range feats {
		at := f.Attributes
		if attrString(at, "FULLSTREET") != street || attrString(at, "MUNCODE") != munCode {
			st.OtherStreet++
			continue
		}
		if f.Geometry == nil || f.Geometry.X == nil || f.Geometry.Y == nil || !validLonLat(*f.Geometry.X, *f.Geometry.Y) {
			st.NoGeometry++
			continue
		}
		num := strings.ToUpper(attrString(at, "MUNNUMBER"))
		if num == "" { // unit points carry the civic number only in FULLADDRES
			if fields := strings.Fields(attrString(at, "FULLADDRES")); len(fields) > 0 {
				num = strings.ToUpper(fields[0])
			}
		}
		if !civicRE.MatchString(num) {
			st.NoNumber++
			continue
		}
		oid, ok := attrInt(at, "OBJECTID_1")
		if !ok {
			return nil, st, fmt.Errorf("site: %s: address point without OBJECTID_1", street)
		}
		unit := strings.ToUpper(attrString(at, "STREET_UNI"))
		key := num + " " + street
		if unit != "" {
			key += " UNIT " + unit
		}
		gid := attrString(at, "GlobalID")
		if gid == "" {
			gid = "oid:" + strconv.FormatInt(oid, 10)
		}
		cands = append(cands, cand{a: Address{Key: key, Number: num, Unit: unit, ObjectID: oid, Lon: *f.Geometry.X, Lat: *f.Geometry.Y}, gid: strings.ToUpper(gid)})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].a.ObjectID < cands[j].a.ObjectID })
	seenGID, seenKey, unitNumbers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var kept []Address
	for _, c := range cands {
		if seenGID[c.gid] {
			st.DuplicateGlobalID++
			continue
		}
		seenGID[c.gid] = true
		if seenKey[c.a.Key] {
			st.DuplicateAddress++
			continue
		}
		seenKey[c.a.Key] = true
		if c.a.Unit != "" {
			unitNumbers[c.a.Number] = true
		}
		kept = append(kept, c.a)
	}
	out := kept[:0]
	for _, a := range kept {
		if a.Unit == "" && unitNumbers[a.Number] {
			st.ParentOfUnits++
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return addressLess(out[i], out[j]) })
	st.Kept = len(out)
	return out, st, nil
}

func leadingInt(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' || n > 1e8 {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func addressLess(a, b Address) bool {
	if x, y := leadingInt(a.Number), leadingInt(b.Number); x != y {
		return x < y
	}
	if a.Number != b.Number {
		return a.Number < b.Number
	}
	if x, y := leadingInt(a.Unit), leadingInt(b.Unit); x != y {
		return x < y
	}
	return a.Unit < b.Unit
}

// normaliseRoads returns one road's built centreline paths, by OBJECTID_1.
func normaliseRoads(pages []json.RawMessage, road, munCode string) ([]RoadPiece, RoadStats, error) {
	var st RoadStats
	feats, err := decodePages(pages)
	if err != nil {
		return nil, st, err
	}
	var pieces []RoadPiece
	for _, f := range feats {
		st.Features++
		at := f.Attributes
		if attrString(at, "FULLNAME") != road {
			st.OtherRoad++
			continue
		}
		if attrString(at, "MUNL") != munCode && attrString(at, "MUNR") != munCode {
			st.OtherMunicipality++
			continue
		}
		if p, ok := attrInt(at, "PROPOSED"); ok && p != 0 {
			st.Proposed++
			continue
		}
		oid, ok := attrInt(at, "OBJECTID_1")
		if !ok {
			return nil, st, fmt.Errorf("site: %s: road piece without OBJECTID_1", road)
		}
		if f.Geometry == nil || len(f.Geometry.Paths) == 0 {
			st.Degenerate++
			continue
		}
		for _, path := range f.Geometry.Paths {
			var pts [][2]float64
			for _, c := range path {
				if len(c) < 2 || !validLonLat(c[0], c[1]) {
					return nil, st, fmt.Errorf("site: %s: road piece %d has a bad vertex %v", road, oid, c)
				}
				v := [2]float64{c[0], c[1]}
				if len(pts) > 0 && pts[len(pts)-1] == v {
					continue
				}
				pts = append(pts, v)
			}
			if len(pts) < 2 {
				st.Degenerate++
				continue
			}
			pieces = append(pieces, RoadPiece{ObjectID: oid, Path: pts})
		}
	}
	sort.SliceStable(pieces, func(i, j int) bool { return pieces[i].ObjectID < pieces[j].ObjectID })
	st.Pieces = len(pieces)
	return pieces, st, nil
}
