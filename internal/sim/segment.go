package sim

import "fmt"

// SegmentKind drives per-segment load multipliers so the simulated
// neighbourhood has the structure §1 of the plan asks about (wooded lots,
// distance to the stormwater pond).
type SegmentKind uint8

// Segment kinds.
const (
	SegmentStandard SegmentKind = iota
	SegmentWooded
	SegmentNearPond
	SegmentHighGround
)

func (k SegmentKind) String() string {
	switch k {
	case SegmentStandard:
		return "standard"
	case SegmentWooded:
		return "wooded"
	case SegmentNearPond:
		return "near_pond"
	case SegmentHighGround:
		return "high_ground"
	}
	return fmt.Sprintf("SegmentKind(%d)", uint8(k))
}

// MarshalText implements encoding.TextMarshaler for JSON output.
func (k SegmentKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// kindMultipliers are applied to every home in a segment of that kind.
var kindMultipliers = map[SegmentKind]struct{ response, baseflow, reservoir float64 }{
	SegmentStandard:   {1.0, 1.0, 1.0},
	SegmentWooded:     {1.3, 1.2, 1.0},
	SegmentNearPond:   {1.6, 2.0, 1.5},
	SegmentHighGround: {0.7, 0.6, 1.0},
}

// kindPattern is the order kinds are assigned to consecutive segments.
var kindPattern = []SegmentKind{
	SegmentStandard, SegmentWooded, SegmentNearPond, SegmentStandard,
	SegmentHighGround, SegmentStandard, SegmentWooded, SegmentNearPond,
}

// SegmentParams describes one street segment of the simulated neighbourhood.
type SegmentParams struct {
	Index        int         `json:"index"`
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Kind         SegmentKind `json:"kind"`
	ResponseMul  float64     `json:"response_mul"`
	BaseflowMul  float64     `json:"baseflow_mul"`
	ReservoirMul float64     `json:"reservoir_mul"`
	HomeIndexes  []int       `json:"home_indexes"`
}

// buildSegments assigns home i to segment i % nSegments (round-robin, so
// adding a home never moves an existing one) and kinds by cycling kindPattern.
func buildSegments(nHomes, nSegments int) []SegmentParams {
	segs := make([]SegmentParams, nSegments)
	for i := range segs {
		kind := kindPattern[i%len(kindPattern)]
		m := kindMultipliers[kind]
		s := SegmentParams{
			Index:        i,
			ID:           fmt.Sprintf("seg-%02d", i+1),
			Name:         fmt.Sprintf("Segment %02d (%s)", i+1, kind),
			Kind:         kind,
			ResponseMul:  m.response,
			BaseflowMul:  m.baseflow,
			ReservoirMul: m.reservoir,
		}
		for h := i; h < nHomes; h += nSegments {
			s.HomeIndexes = append(s.HomeIndexes, h)
		}
		segs[i] = s
	}
	return segs
}
