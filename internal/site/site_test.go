package site

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/sim"
)

// The fixture is a synthetic town ("Testville", MUNCODE TSTV) around an
// arbitrary origin. No real address, street or coordinate appears in tests.

var testOrigin = newProjection(-79.5, 44.0)

func ll(x, y float64) [2]float64 {
	lon, lat := testOrigin.inv(xy{x, y})
	return [2]float64{lon, lat}
}

type fakeFeature struct {
	oid   int64
	attrs map[string]any
	geom  map[string]any
}

type fakeArcGIS struct {
	mu       sync.Mutex
	addr     []fakeFeature
	road     []fakeFeature
	requests int
	fail     int // respond 503 to this many requests first
}

var (
	addrWhere = regexp.MustCompile(`^FULLSTREET = '([^']*)' AND MUNCODE = '([^']*)'$`)
	roadWhere = regexp.MustCompile(`^FULLNAME = '([^']*)' AND \(MUNL = '([^']*)' OR MUNR = '([^']*)'\)$`)
)

func (f *fakeArcGIS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if f.fail > 0 {
		f.fail--
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	bad := func(msg string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 400, "message": msg}})
	}
	if q.Get("outSR") != "4326" || q.Get("f") != "json" || q.Get("orderByFields") != "OBJECTID_1" {
		bad("unexpected parameters")
		return
	}
	var matches []fakeFeature
	where := q.Get("where")
	switch r.URL.Path {
	case "/addr/MapServer/1/query":
		m := addrWhere.FindStringSubmatch(where)
		if m == nil {
			bad("Failed to execute query.")
			return
		}
		for _, ft := range f.addr {
			if ft.attrs["FULLSTREET"] == m[1] && ft.attrs["MUNCODE"] == m[2] {
				matches = append(matches, ft)
			}
		}
	case "/road/MapServer/0/query":
		m := roadWhere.FindStringSubmatch(where)
		if m == nil {
			bad("Failed to execute query.")
			return
		}
		for _, ft := range f.road {
			if ft.attrs["FULLNAME"] == m[1] && (ft.attrs["MUNL"] == m[2] || ft.attrs["MUNR"] == m[3]) {
				matches = append(matches, ft)
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].oid < matches[j].oid })
	off, _ := strconv.Atoi(q.Get("resultOffset"))
	n, _ := strconv.Atoi(q.Get("resultRecordCount"))
	end := min(off+n, len(matches))
	var feats []map[string]any
	for _, ft := range matches[min(off, end):end] {
		feats = append(feats, map[string]any{"attributes": ft.attrs, "geometry": ft.geom})
	}
	resp := map[string]any{"spatialReference": map[string]any{"wkid": 4326, "latestWkid": 4326}, "features": feats}
	if end < len(matches) {
		resp["exceededTransferLimit"] = true
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func addrFeature(oid int64, gid, number, unit, street string, p [2]float64) fakeFeature {
	full := number + " " + street
	if strings.TrimSpace(number) == "" {
		full = "7 " + street // the County's unit rows repeat the parent civic number here
	}
	return fakeFeature{oid: oid, attrs: map[string]any{
		"OBJECTID_1": oid, "GlobalID": gid, "MUNNUMBER": number, "STREET_UNI": unit, "FULLADDRES": full,
		"FULLSTREET": street, "MUNCODE": "TSTV",
	}, geom: map[string]any{"x": p[0], "y": p[1]}}
}

func roadFeature(oid int64, name string, proposed int, pts ...[2]float64) fakeFeature {
	path := make([][]float64, len(pts))
	for i, p := range pts {
		path[i] = []float64{p[0], p[1]}
	}
	return fakeFeature{oid: oid, attrs: map[string]any{
		"OBJECTID_1": oid, "FULLNAME": name, "MUNL": "TSTV", "MUNR": "TSTV", "PROPOSED": proposed,
	}, geom: map[string]any{"paths": [][][]float64{path}}}
}

// elmArc is Elm Crescent's centreline: a quarter circle of radius 150 m.
func elmArc() [][2]float64 {
	var pts [][2]float64
	for k := 0; k <= 6; k++ {
		a := float64(15*k) * math.Pi / 180
		pts = append(pts, ll(150*math.Cos(a), 150*math.Sin(a)))
	}
	return pts
}

func newFixture() *fakeArcGIS {
	f := &fakeArcGIS{}
	arc := elmArc()
	f.road = []fakeFeature{
		roadFeature(30, "ELM CRES", 0, arc[4], arc[5], arc[6]),
		roadFeature(10, "ELM CRES", 0, arc[2], arc[1], arc[0]), // reversed, out of order
		roadFeature(20, "ELM CRES", 0, arc[2], arc[3], arc[4]),
		roadFeature(40, "OAKLANE", 0, ll(300, 0), ll(300, 60)),
		roadFeature(41, "OAKLANE", 0, ll(300, 60), ll(300, 120)),
		roadFeature(42, "OAKLANE", 1, ll(300, 120), ll(300, 300)), // proposed, not built
	}
	for k := 0; k < 10; k++ {
		a := (4.5 + 9*float64(k)) * math.Pi / 180
		r := 125.0
		if k%2 == 1 {
			r = 175
		}
		f.addr = append(f.addr, addrFeature(int64(100+k), "{E"+strconv.Itoa(k)+"}", strconv.Itoa(2*k+1), " ", "ELM CRES", ll(r*math.Cos(a), r*math.Sin(a))))
	}
	f.addr = append(f.addr,
		addrFeature(200, "{O0}", "5", " ", "OAK LANE", ll(325, 30)),
		addrFeature(201, "{O1}", "5", " ", "OAK LANE", ll(326, 31)), // same address again
		addrFeature(202, "{O0}", "5", " ", "OAK LANE", ll(325, 30)), // same GlobalID again
		addrFeature(203, "{O3}", "7", " ", "OAK LANE", ll(275, 90)), // parent of the units below
		addrFeature(204, "{O4}", " ", "1", "OAK LANE", ll(272, 84)),
		addrFeature(205, "{O5}", " ", "2", "OAK LANE", ll(272, 96)),
		addrFeature(206, "{O6}", "9", " ", "OAK LANE", ll(325, 110)),
	)
	return f
}

var (
	elm = StreetConfig{Name: "ELM CRES", Label: "Elm Crescent", Kind: "wooded"}
	oak = StreetConfig{Name: "OAK LANE", RoadName: "OAKLANE", Label: "Oak Lane", Kind: "standard"}
)

func testConfig(streets ...StreetConfig) Config {
	return Config{Name: "testville", Description: "synthetic", MunCode: "TSTV", DefaultKind: "standard", MaxHomesPerSegment: 4, Streets: streets}
}

var testSalt = []byte("0123456789abcdef-test-salt")

func testSource(srvURL, dir string) *Source {
	return &Source{
		Client: &Client{HTTP: http.DefaultClient, PageSize: 3, Retries: 2, Backoff: time.Millisecond},
		Dir:    dir, AddressURL: srvURL + "/addr/MapServer/1", RoadURL: srvURL + "/road/MapServer/0",
		Now: func() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) },
	}
}

func importFixture(t *testing.T, f *fakeArcGIS, dir string, cfg Config) *Snapshot {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	im := &Importer{Source: testSource(srv.URL, dir), Salt: testSalt, Now: time.Now}
	snap, err := im.Import(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestImportNormalises(t *testing.T) {
	f := newFixture()
	f.fail = 1 // the first request is retried
	dir := t.TempDir()
	snap := importFixture(t, f, dir, testConfig(elm, oak))
	if len(snap.Streets) != 2 {
		t.Fatalf("streets = %d", len(snap.Streets))
	}
	e, o := snap.Streets[0], snap.Streets[1]
	if e.AddressStats.Kept != 10 || e.AddressStats.Raw != 10 {
		t.Errorf("elm address stats %+v", e.AddressStats)
	}
	wantOak := AddressStats{Raw: 7, Kept: 4, DuplicateGlobalID: 1, DuplicateAddress: 1, ParentOfUnits: 1}
	if o.AddressStats != wantOak {
		t.Errorf("oak address stats %+v, want %+v", o.AddressStats, wantOak)
	}
	var keys []string
	for _, a := range o.Addresses {
		keys = append(keys, a.Key)
	}
	if got := strings.Join(keys, "|"); got != "5 OAK LANE|7 OAK LANE UNIT 1|7 OAK LANE UNIT 2|9 OAK LANE" {
		t.Errorf("oak keys %s", got)
	}
	if o.Addresses[0].ObjectID != 200 {
		t.Errorf("duplicate kept object %d, want the lowest (200)", o.Addresses[0].ObjectID)
	}
	if o.RoadStats.Proposed != 1 || o.RoadStats.Pieces != 2 || len(o.Centreline) != 1 || len(o.Centreline[0]) != 3 {
		t.Errorf("oak roads %+v, centreline %v", o.RoadStats, o.Centreline)
	}
	arc := elmArc()
	if len(e.Centreline) != 1 || len(e.Centreline[0]) != len(arc) {
		t.Fatalf("elm centreline %v", e.Centreline)
	}
	for k, v := range e.Centreline[0] {
		if v != arc[k] {
			t.Errorf("elm centreline vertex %d = %v, want %v (exact source coordinates, chained)", k, v, arc[k])
		}
	}
	if e.AddressSource.Pages != 4 || e.AddressSource.FromCache || e.AddressSource.Query["outSR"] != "4326" {
		t.Errorf("elm address source %+v", e.AddressSource)
	}
	path := filepath.Join(dir, "snap.json")
	if err := snap.Write(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(snap)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Error("snapshot does not round-trip")
	}
}

func TestBuild(t *testing.T) {
	snap := importFixture(t, newFixture(), t.TempDir(), testConfig(elm, oak))
	built, err := Build(snap)
	if err != nil {
		t.Fatal(err)
	}
	type segWant struct {
		id    string
		homes int
		kind  sim.SegmentKind
	}
	want := []segWant{{"elm-cres-1", 4, sim.SegmentWooded}, {"elm-cres-2", 3, sim.SegmentWooded}, {"elm-cres-3", 3, sim.SegmentWooded}, {"oak-lane-1", 4, sim.SegmentStandard}}
	if len(built.Segments) != len(want) || len(built.Homes) != 14 {
		t.Fatalf("%d segments, %d homes", len(built.Segments), len(built.Homes))
	}
	pr := testOrigin
	rings := make([][]xy, len(built.Segments))
	for i, s := range built.Segments {
		if s.ID != want[i].id || s.Homes != want[i].homes || s.Kind != want[i].kind || s.HomesOutside != 0 {
			t.Errorf("segment %d = %s homes %d kind %s outside %d", i, s.ID, s.Homes, s.Kind, s.HomesOutside)
		}
		if i == 1 && s.Name != "Elm Crescent (part 2 of 3)" {
			t.Errorf("segment name %q", s.Name)
		}
		rings[i] = parsePolygon(t, s.Geometry, pr)
	}
	// Blocks are contiguous along the centreline, and every home lies in its
	// own segment's outline and no other.
	for _, h := range built.Homes {
		p := pr.fwd(h.Lon, h.Lat)
		for i, ring := range rings {
			if in := pointInRing(p, ring); in != (i == h.Segment) {
				t.Errorf("home in segment %d: inside segment %d outline = %v", h.Segment, i, in)
			}
		}
	}
	var elmMeasures [3][]float64
	for _, h := range built.Homes {
		if h.Segment < 3 {
			elmMeasures[h.Segment] = append(elmMeasures[h.Segment], h.Measure)
		}
	}
	for s := 0; s < 2; s++ {
		if slicesMax(elmMeasures[s]) >= slicesMin(elmMeasures[s+1]) {
			t.Errorf("block %d overlaps block %d along the street", s+1, s+2)
		}
	}
	for i := 1; i < len(built.Homes); i++ {
		if built.Homes[i-1].HomeID >= built.Homes[i].HomeID {
			t.Fatal("homes are not ordered by home id")
		}
	}
	devEUI := regexp.MustCompile(`^5e[0-9a-f]{14}$`)
	for _, h := range built.Homes {
		if !devEUI.MatchString(h.DevEUI) || h.Stream&(1<<63) == 0 || len(h.DevAddr) != 8 {
			t.Errorf("identity %s %s %x", h.DevEUI, h.DevAddr, h.Stream)
		}
	}
	if i, ok := built.Locate("  7 oak lane   unit 2 "); !ok || built.Homes[i].Address != "7 OAK LANE UNIT 2" {
		t.Errorf("Locate: %d %v", i, ok)
	}
	if _, ok := built.Locate("11 OAK LANE"); ok {
		t.Error("Locate found a missing address")
	}

	// Deterministic.
	again, err := Build(snap)
	if err != nil {
		t.Fatal(err)
	}
	for i := range built.Segments {
		if string(again.Segments[i].Geometry) != string(built.Segments[i].Geometry) {
			t.Fatalf("segment %s geometry differs between builds", built.Segments[i].ID)
		}
	}

	// The simulator runs the site, and nothing it holds or exports names an address.
	site := built.SimSite()
	e, err := sim.New(sim.Config{Seed: 7, Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Scenario: sim.Scenarios()["storm25"], Duration: 3 * time.Hour, Site: site, RainGauges: 2})
	if err != nil {
		t.Fatal(err)
	}
	truth, err := e.Run(context.Background(), sim.NewHashSink())
	if err != nil || truth.Events == 0 {
		t.Fatalf("run: %v, %d events", err, truth.Events)
	}
	for name, v := range map[string]any{"sim site": site, "truth": truth} {
		b, merr := json.Marshal(v)
		if merr != nil {
			t.Fatal(merr)
		}
		for _, street := range []string{"ELM CRES", "OAK LANE", "OAKLANE"} {
			if strings.Contains(string(b), street) {
				t.Errorf("%s JSON contains %q", name, street)
			}
		}
	}
	for _, h := range truth.Homes {
		b, _ := json.Marshal(h)
		if strings.Contains(string(b), "lon") || strings.Contains(string(b), "lat") {
			t.Errorf("truth home carries coordinates: %s", b)
		}
	}
	fc, err := built.SegmentsGeoJSON()
	if err != nil || !json.Valid(fc) || strings.Contains(string(fc), "OAK LANE") {
		t.Errorf("segments geojson: %v", err)
	}
}

func slicesMax(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

func slicesMin(v []float64) float64 {
	m := math.Inf(1)
	for _, x := range v {
		m = math.Min(m, x)
	}
	return m
}

// parsePolygon checks a GeoJSON Polygon is valid (one closed, simple,
// counter-clockwise ring) and returns it projected, without the closing vertex.
func parsePolygon(t *testing.T, geo json.RawMessage, pr projection) []xy {
	t.Helper()
	var g struct {
		Type        string         `json:"type"`
		Coordinates [][][2]float64 `json:"coordinates"`
	}
	if err := json.Unmarshal(geo, &g); err != nil || g.Type != "Polygon" || len(g.Coordinates) != 1 {
		t.Fatalf("geometry %s: %v", geo, err)
	}
	ring := g.Coordinates[0]
	if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
		t.Fatalf("ring not closed: %v", ring)
	}
	ll := make([]xy, len(ring)-1)
	proj := make([]xy, len(ring)-1)
	for k := range ll {
		ll[k] = xy{ring[k][0], ring[k][1]}
		proj[k] = pr.fwd(ring[k][0], ring[k][1])
	}
	if !ringSimple(ll) || signedArea(ll) <= 0 {
		t.Fatalf("ring not simple or not counter-clockwise (area %g)", signedArea(ll))
	}
	return proj
}

// A home's identity depends only on its address and the salt, so it is the
// same in every site that includes its street.
func TestIdentityStableAcrossSites(t *testing.T) {
	f := newFixture()
	both, err := Build(importFixture(t, f, t.TempDir(), testConfig(elm, oak)))
	if err != nil {
		t.Fatal(err)
	}
	oakOnly, err := Build(importFixture(t, f, t.TempDir(), testConfig(oak)))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range oakOnly.Homes {
		i, ok := both.Locate(h.Address)
		if !ok {
			t.Fatalf("home missing from the larger site")
		}
		g := both.Homes[i]
		if g.HomeID != h.HomeID || g.DevEUI != h.DevEUI || g.DevAddr != h.DevAddr || g.Stream != h.Stream {
			t.Errorf("identity differs between sites")
		}
	}
	h1, e1, _, s1 := identity(testSalt, "TSTV", "5 OAK LANE")
	h2, e2, _, s2 := identity([]byte("another-salt-of-16+bytes"), "TSTV", "5 OAK LANE")
	if h1 == h2 || e1 == e2 || s1 == s2 {
		t.Error("a different salt gives the same identity")
	}
	if strings.Contains(h1+e1, "5") && strings.Contains(h1, "OAK") {
		t.Error("identity embeds address text")
	}
}

func TestSourceCacheOfflineRefresh(t *testing.T) {
	f := newFixture()
	dir := t.TempDir()
	cfg := testConfig(elm, oak)
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, err := (&Importer{Source: testSource(srv.URL, dir), Salt: testSalt}).Import(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	fetched := f.requests
	f.mu.Unlock()

	// Offline from the cache alone, with a client that cannot reach anything.
	src := testSource(srv.URL, dir)
	src.Client.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("offline import made a request")
		return nil, errors.New("offline")
	})}
	src.Offline = true
	snap, err := (&Importer{Source: src, Salt: testSalt}).Import(context.Background(), cfg)
	if err != nil {
		t.Fatalf("offline import: %v", err)
	}
	if !snap.Streets[0].AddressSource.FromCache || !snap.Streets[1].RoadSource.FromCache {
		t.Error("offline import did not use the cache")
	}
	if _, err := (&Importer{Source: src, Salt: testSalt}).Import(context.Background(), testConfig(StreetConfig{Name: "PINE ST", Label: "Pine Street", Kind: "standard"})); err == nil || !strings.Contains(err.Error(), "not cached") {
		t.Errorf("offline import of an uncached street: %v", err)
	}
	other := cfg
	other.MunCode = "ELSE"
	if _, err := (&Importer{Source: src, Salt: testSalt}).Import(context.Background(), other); err == nil || !strings.Contains(err.Error(), "different source or query") {
		t.Errorf("offline import with another query: %v", err)
	}

	// An online import with the same source reads the cache; refresh re-queries.
	online := testSource(srv.URL, dir)
	if _, err := (&Importer{Source: online, Salt: testSalt}).Import(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.requests != fetched {
		t.Errorf("cached import made %d requests", f.requests-fetched)
	}
	f.mu.Unlock()
	online.Refresh = true
	if _, err := (&Importer{Source: online, Salt: testSalt}).Import(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.requests <= fetched {
		t.Error("refresh made no requests")
	}
	f.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "roads", "oaklane.json")); err != nil {
		t.Errorf("road cache file: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestImportErrors(t *testing.T) {
	f := newFixture()
	srv := httptest.NewServer(f)
	defer srv.Close()
	im := &Importer{Source: testSource(srv.URL, t.TempDir()), Salt: testSalt}
	if _, err := im.Import(context.Background(), testConfig(StreetConfig{Name: "PINE ST", Label: "Pine Street", Kind: "standard"})); err == nil || !strings.Contains(err.Error(), "no address points") {
		t.Errorf("unknown street: %v", err)
	}
	noAlias := oak
	noAlias.RoadName = ""
	if _, err := im.Import(context.Background(), testConfig(noAlias)); err == nil || !strings.Contains(err.Error(), "road_name") {
		t.Errorf("road spelled differently: %v", err)
	}
	if _, err := (&Importer{Source: im.Source, Salt: []byte("short")}).Import(context.Background(), testConfig(oak)); err == nil {
		t.Error("short salt accepted")
	}
}

func TestSalt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c", "salt.hex")
	a, created, err := LoadOrCreateSalt(p)
	if err != nil || !created || len(a) != 32 {
		t.Fatalf("create: %v %v %d", err, created, len(a))
	}
	b, created, err := LoadOrCreateSalt(p)
	if err != nil || created || string(a) != string(b) {
		t.Fatalf("reload: %v %v", err, created)
	}
	if err := os.WriteFile(p, []byte("abcd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateSalt(p); err == nil {
		t.Error("short salt accepted")
	}
}
