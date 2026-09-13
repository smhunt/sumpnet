package site

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smhunt/sumpnet/internal/sim"
)

func TestEmbeddedConfigs(t *testing.T) {
	want := map[string]int{"timberwalk": 7, "timberwalk-plus-basil-bowman": 10, "timberwalk-plus-plant-streets": 14, "timberwalk-nearby": 19}
	names := ConfigNames()
	if len(names) != len(want) {
		t.Errorf("configs %v", names)
	}
	core, err := LoadConfig("timberwalk")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		c, err := LoadConfig(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if len(c.Streets) != want[n] || c.MunCode != "MIDC" || c.MaxHomesPerSegment != 40 {
			t.Errorf("%s: %d streets, muncode %q, max %d", n, len(c.Streets), c.MunCode, c.MaxHomesPerSegment)
		}
		for i, s := range core.Streets { // every larger site starts with the core streets, unchanged
			if c.Streets[i] != s {
				t.Errorf("%s street %d = %+v, want %+v", n, i, c.Streets[i], s)
			}
		}
		for _, s := range c.Streets[len(core.Streets):] {
			if s.Kind != "standard" {
				t.Errorf("%s: %s kind %q", n, s.Name, s.Kind)
			}
		}
	}
	if core.Streets[0].Road() != "ARROWWOODPATH" || core.Streets[1].Road() != "MAYAPPLE CRES" || core.Streets[0].Kind != "wooded" {
		t.Errorf("core streets %+v", core.Streets[:2])
	}
	nearby, _ := LoadConfig("timberwalk-nearby")
	if strings.Join(nearby.Plans, ",") != "39T-MC0401,39T-MC1901,39T-MC1401,39T-MC0601,39T-MC0602" {
		t.Errorf("plans %v", nearby.Plans)
	}
}

func TestConfigErrors(t *testing.T) {
	files := map[string]string{
		"a":         `{"name":"a","extends":"b","streets":[{"name":"X ST","label":"X"}]}`,
		"b":         `{"name":"b","extends":"a","muncode":"M","streets":[{"name":"Y ST","label":"Y"}]}`,
		"dup":       `{"name":"dup","muncode":"M","streets":[{"name":"X ST","label":"X"},{"name":"X ST","label":"X2"}]}`,
		"kind":      `{"name":"kind","muncode":"M","streets":[{"name":"X ST","label":"X","kind":"swampy"}]}`,
		"mismatch":  `{"name":"other","muncode":"M","streets":[{"name":"X ST","label":"X"}]}`,
		"nomun":     `{"name":"nomun","streets":[{"name":"X ST","label":"X"}]}`,
		"lower":     `{"name":"lower","muncode":"M","streets":[{"name":"x st","label":"X"}]}`,
		"nolabel":   `{"name":"nolabel","muncode":"M","streets":[{"name":"X ST"}]}`,
		"nostreets": `{"name":"nostreets","muncode":"M","streets":[]}`,
		"child":     `{"name":"child","extends":"base","streets":[{"name":"Y ST","label":"Y"}]}`,
		"base":      `{"name":"base","muncode":"M","default_kind":"wooded","streets":[{"name":"X ST","label":"X"}]}`,
	}
	read := func(n string) ([]byte, error) {
		if s, ok := files[n]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
	for name, want := range map[string]string{
		"a": "extends itself", "dup": "twice", "kind": "unknown segment kind", "mismatch": "names itself",
		"nomun": "no muncode", "lower": "upper-case", "nolabel": "no label", "nostreets": "no streets", "missing": "no site config", "Bad Name": "bad site name",
	} {
		if _, err := loadConfig(name, read, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	c, err := loadConfig("child", read, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The child's own streets take the child's default kind (standard), the
	// parent's keep theirs; muncode is inherited.
	if c.MunCode != "M" || c.Streets[0].Kind != "wooded" || c.Streets[1].Kind != "wooded" {
		t.Errorf("child = %+v", c)
	}
}

func TestClientErrors(t *testing.T) {
	var calls atomic.Int32
	mode := "arcgis-400"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		switch mode {
		case "arcgis-400":
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Failed to execute query.","details":[]}}`))
		case "arcgis-503-then-ok":
			if n == 1 {
				_, _ = w.Write([]byte(`{"error":{"code":503,"message":"busy"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"features":[]}`))
		case "http-404":
			http.NotFound(w, nil)
		case "garbage":
			_, _ = w.Write([]byte(`{"features":`))
		case "empty-exceeded":
			_, _ = w.Write([]byte(`{"features":[],"exceededTransferLimit":true}`))
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), Retries: 2, Backoff: time.Millisecond}
	q := AddressQuery("X ST", "M")
	for _, tt := range []struct {
		mode      string
		wantErr   string
		wantCalls int32
	}{
		{"arcgis-400", "ArcGIS error 400", 1},
		{"arcgis-503-then-ok", "", 2},
		{"http-404", "404", 1},
		{"garbage", "decode", 3},
		{"empty-exceeded", "empty page", 1},
	} {
		mode = tt.mode
		calls.Store(0)
		pages, err := c.Fetch(context.Background(), srv.URL+"/layer/1", q)
		if tt.wantErr == "" && (err != nil || len(pages) != 1) {
			t.Errorf("%s: %v, %d pages", tt.mode, err, len(pages))
		}
		if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%s: %v, want %q", tt.mode, err, tt.wantErr)
		}
		if calls.Load() != tt.wantCalls {
			t.Errorf("%s: %d calls, want %d", tt.mode, calls.Load(), tt.wantCalls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mode = "garbage"
	if _, err := c.Fetch(ctx, srv.URL+"/layer/1", q); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled fetch: %v", err)
	}
	if _, err := c.Fetch(context.Background(), "not a url", q); err == nil {
		t.Error("bad URL accepted")
	}
	if got := AddressQuery("O'NEIL ST", "M").Where; got != "FULLSTREET = 'O''NEIL ST' AND MUNCODE = 'M'" {
		t.Errorf("quoting: %s", got)
	}
}

func TestNormaliseRejects(t *testing.T) {
	notWGS := json.RawMessage(`{"spatialReference":{"wkid":26917},"features":[{"attributes":{},"geometry":{"x":1,"y":2}}]}`)
	if _, _, err := normaliseAddresses([]json.RawMessage{notWGS}, "X ST", "M"); err == nil {
		t.Error("projected page accepted")
	}
	page := json.RawMessage(`{"spatialReference":{"wkid":4326},"features":[
		{"attributes":{"OBJECTID_1":1,"FULLSTREET":"X ST","MUNCODE":"M","MUNNUMBER":" ","STREET_UNI":" ","FULLADDRES":"  X ST"},"geometry":{"x":-79,"y":44}},
		{"attributes":{"OBJECTID_1":2,"FULLSTREET":"X ST","MUNCODE":"M","MUNNUMBER":"4"},"geometry":null},
		{"attributes":{"OBJECTID_1":3,"FULLSTREET":"Y ST","MUNCODE":"M","MUNNUMBER":"4"},"geometry":{"x":-79,"y":44}},
		{"attributes":{"OBJECTID_1":4,"FULLSTREET":"X ST","MUNCODE":"M","MUNNUMBER":"12a"},"geometry":{"x":-79,"y":44}}
	]}`)
	addrs, st, err := normaliseAddresses([]json.RawMessage{page}, "X ST", "M")
	if err != nil || len(addrs) != 1 || addrs[0].Key != "12A X ST" || st.NoNumber != 1 || st.NoGeometry != 1 || st.OtherStreet != 1 {
		t.Errorf("addresses %+v stats %+v err %v", addrs, st, err)
	}
	roads := json.RawMessage(`{"spatialReference":{"wkid":4326},"features":[
		{"attributes":{"OBJECTID_1":9,"FULLNAME":"X ST","MUNL":"Q","MUNR":"Q","PROPOSED":0},"geometry":{"paths":[[[-79,44],[-79.001,44]]]}},
		{"attributes":{"OBJECTID_1":8,"FULLNAME":"X ST","MUNL":"M","MUNR":"Q","PROPOSED":0},"geometry":{"paths":[[[-79,44],[-79,44]]]}}
	]}`)
	pieces, rst, err := normaliseRoads([]json.RawMessage{roads}, "X ST", "M")
	if err != nil || len(pieces) != 0 || rst.OtherMunicipality != 1 || rst.Degenerate != 1 {
		t.Errorf("roads %+v stats %+v err %v", pieces, rst, err)
	}
}

func TestChainOrder(t *testing.T) {
	a, b, c, d := xy{0, 0}, xy{100, 0}, xy{100, 100}, xy{0, 100}
	// A closed loop with no free end, one piece reversed.
	loop := chainOrder([][]xy{{a, b}, {c, b}, {c, d}, {d, a}}, 1)
	if len(loop) != 1 || len(loop[0]) != 4 {
		t.Fatalf("loop chains %v", loop)
	}
	// Two disjoint lines, and a piece whose joint is 0.5 m off.
	lines := chainOrder([][]xy{{a, b}, {xy{500, 0}, xy{600, 0}}, {xy{100.5, 0}, c}}, 1)
	if len(lines) != 2 || len(lines[0]) != 2 || len(lines[1]) != 1 {
		t.Fatalf("chains %v", lines)
	}
	cl := newCentreline([][]xy{{a, b, c}})
	if cl.length != 200 {
		t.Fatalf("length %v", cl.length)
	}
	m, foot, d2 := cl.locate(xy{130, 40})
	if m != 140 || foot != (xy{100, 40}) || d2 != 30 {
		t.Errorf("locate = %v %v %v", m, foot, d2)
	}
	parts := cl.slice(50, 150)
	if len(parts) != 1 || len(parts[0]) != 3 || parts[0][0] != (xy{50, 0}) || parts[0][2] != (xy{100, 50}) {
		t.Errorf("slice %v", parts)
	}
}

func TestRingPredicates(t *testing.T) {
	square := []xy{{0, 0}, {10, 0}, {10, 10}, {0, 10}}
	if !ringSimple(square) || signedArea(square) != 100 {
		t.Error("square")
	}
	for name, r := range map[string][]xy{
		"bow tie":   {{0, 0}, {10, 10}, {10, 0}, {0, 10}},
		"fold back": {{0, 0}, {10, 0}, {5, 0}, {5, 5}},
		"repeat":    {{0, 0}, {10, 0}, {10, 0}, {0, 10}},
		"touching":  {{0, 0}, {10, 0}, {10, 10}, {5, 0}, {0, 10}},
		"two":       {{0, 0}, {1, 1}},
	} {
		if ringSimple(r) {
			t.Errorf("%s accepted", name)
		}
	}
	if !pointInRing(xy{5, 5}, square) || pointInRing(xy{15, 5}, square) {
		t.Error("pointInRing")
	}
}

func TestTraceRegion(t *testing.T) {
	// A 5x5 frame with a hole, plus a cell touching it only diagonally.
	const w, h = 9, 9
	in := make([]bool, w*h)
	for j := 1; j <= 5; j++ {
		for i := 1; i <= 5; i++ {
			in[j*w+i] = i == 1 || i == 5 || j == 1 || j == 5
		}
	}
	in[6*w+6] = true
	keep := largestComponent(in, w, h)
	if keep[6*w+6] {
		t.Error("diagonal-only cell kept")
	}
	fillHoles(keep, w, h)
	if !keep[3*w+3] {
		t.Error("hole not filled")
	}
	ring, err := traceRing(keep, w, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(ring) != 4 || signedArea(ring) != 25 {
		t.Errorf("ring %v area %v", ring, signedArea(ring))
	}
	pinch := make([]bool, w*h)
	pinch[1*w+1], pinch[2*w+2] = true, true
	if _, err := traceRing(pinch, w, h); err == nil {
		t.Error("saddle accepted by traceRing")
	}
}

func TestOutlinesDoNotOverlap(t *testing.T) {
	caps := []capsule{
		{a: xy{0, 0}, b: xy{200, 0}, label: 0},
		{a: xy{0, 24}, b: xy{200, 24}, label: 1}, // parallel street 24 m away
		{a: xy{100, -20}, b: xy{100, 0}, label: 0},
		{a: xy{300, 300}, b: xy{300, 300}, label: 2}, // a point
	}
	rings, err := outlines(caps, 4, defaultOutline)
	if err != nil {
		t.Fatal(err)
	}
	if rings[3] != nil {
		t.Error("empty label got a ring")
	}
	for l := 0; l < 3; l++ {
		if !ringSimple(rings[l]) || signedArea(rings[l]) <= 0 {
			t.Fatalf("label %d ring invalid", l)
		}
	}
	for x := 5.0; x < 200; x += 10 {
		for y := -40.0; y < 60; y += 3 {
			if pointInRing(xy{x, y}, rings[0]) && pointInRing(xy{x, y}, rings[1]) {
				// Only simplification (1.5 m) can make outlines meet.
				if y < 12-2 || y > 12+2 {
					t.Errorf("(%v, %v) is inside both outlines", x, y)
				}
			}
		}
	}
	if !pointInRing(xy{100, -20}, rings[0]) || !pointInRing(xy{300, 300}, rings[2]) {
		t.Error("capsule endpoints outside their outline")
	}
}

func TestParseKind(t *testing.T) {
	for _, k := range []sim.SegmentKind{sim.SegmentStandard, sim.SegmentWooded, sim.SegmentNearPond, sim.SegmentHighGround} {
		if got, err := ParseKind(k.String()); err != nil || got != k {
			t.Errorf("%s: %v %v", k, got, err)
		}
	}
	if _, err := ParseKind("marsh"); err == nil {
		t.Error("unknown kind accepted")
	}
}
