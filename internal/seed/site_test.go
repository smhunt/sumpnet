package seed

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/smhunt/sumpnet/internal/sim"
	"github.com/smhunt/sumpnet/internal/site"
)

// testSite is a synthetic built site (no real addresses or coordinates).
func testSite() *site.Built {
	b := &site.Built{Name: "testville", MunCode: "TSTV"}
	square := func(x float64) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"type":"Polygon","coordinates":[[[%[1]g,44],[%[2]g,44],[%[2]g,44.001],[%[1]g,44.001],[%[1]g,44]]]}`, x, x+0.001))
	}
	for s, street := range []string{"ELM CRES", "OAK LANE"} {
		b.Segments = append(b.Segments, site.Segment{ID: fmt.Sprintf("%s-1", strings.ToLower(strings.ReplaceAll(street, " ", "-"))),
			Name: street, Kind: sim.SegmentWooded, Street: street, Block: 1, Blocks: 1, Homes: 3, Geometry: square(-79.5 + 0.01*float64(s))})
		for k := 0; k < 3; k++ {
			n := s*3 + k
			b.Homes = append(b.Homes, site.Home{
				Address: fmt.Sprintf("%d %s", 2*k+1, street), Street: street, Segment: s, Lon: -79.5 + 0.01*float64(s), Lat: 44,
				HomeID: fmt.Sprintf("10000000-0000-5000-8000-%012x", n+1), DevEUI: fmt.Sprintf("5e00000000%06x", n+1),
				DevAddr: fmt.Sprintf("02%06x", n+1), Stream: 1<<63 | uint64(n+1),
			})
		}
	}
	return b
}

func TestApplySiteValidates(t *testing.T) {
	for name, o := range map[string]Options{
		"owner without address": {Seed: 1, Site: testSite(), OwnerSubject: "user_x"},
		"unknown address":       {Seed: 1, Site: testSite(), OwnerSubject: "user_x", OwnerAddress: "99 ELM CRES"},
		"address without site":  {Seed: 1, Homes: 4, Segments: 2, OwnerSubject: "user_x", OwnerAddress: "1 ELM CRES"},
	} {
		if _, err := Apply(t.Context(), nil, o); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "99 ELM") {
			t.Errorf("%s: error echoes the address: %v", name, err)
		}
	}
}
