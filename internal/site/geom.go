package site

import (
	"errors"
	"fmt"
	"math"
)

// Pure planar geometry for site outlines. Coordinates are projected to a
// local equirectangular plane in metres around the site centroid, which is
// accurate to well under a metre over a few kilometres.

type xy struct{ X, Y float64 }

const earthRadiusM = 6_371_008.8

type projection struct{ lon0, lat0, kx, ky float64 }

func newProjection(lon0, lat0 float64) projection {
	ky := earthRadiusM * math.Pi / 180
	return projection{lon0: lon0, lat0: lat0, kx: ky * math.Cos(lat0*math.Pi/180), ky: ky}
}

func (p projection) fwd(lon, lat float64) xy { return xy{(lon - p.lon0) * p.kx, (lat - p.lat0) * p.ky} }

func (p projection) inv(q xy) (lon, lat float64) { return p.lon0 + q.X/p.kx, p.lat0 + q.Y/p.ky }

func dist(a, b xy) float64 { return math.Hypot(a.X-b.X, a.Y-b.Y) }

func cross(o, a, b xy) float64 { return (a.X-o.X)*(b.Y-o.Y) - (a.Y-o.Y)*(b.X-o.X) }

// closestOnSegment returns the parameter t in [0, 1] of the point of ab
// nearest to p, that point, and the distance to it.
func closestOnSegment(p, a, b xy) (float64, xy, float64) {
	dx, dy := b.X-a.X, b.Y-a.Y
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = math.Max(0, math.Min(1, ((p.X-a.X)*dx+(p.Y-a.Y)*dy)/l2))
	}
	f := xy{a.X + t*dx, a.Y + t*dy}
	return t, f, dist(p, f)
}

// pieceRef is one road piece in a chain, possibly walked end to start.
type pieceRef struct {
	index    int
	reversed bool
}

// chainOrder joins pieces whose endpoints meet (within tolM) into as few
// chains as possible. Chains start at a free end when there is one; pieces
// are taken in index order, so the result is deterministic.
func chainOrder(pieces [][]xy, tolM float64) [][]pieceRef {
	used := make([]bool, len(pieces))
	first := func(i int) xy { return pieces[i][0] }
	last := func(i int) xy { return pieces[i][len(pieces[i])-1] }
	touches := func(i int, p xy) bool {
		for j := range pieces {
			if j != i && !used[j] && (dist(p, first(j)) <= tolM || dist(p, last(j)) <= tolM) {
				return true
			}
		}
		return false
	}
	find := func(p xy) (int, bool) { // index, and whether its start touches p
		for j := range pieces {
			if used[j] {
				continue
			}
			if dist(p, first(j)) <= tolM {
				return j, true
			}
			if dist(p, last(j)) <= tolM {
				return j, false
			}
		}
		return -1, false
	}
	var chains [][]pieceRef
	for {
		start, rev := -1, false
		for i := range pieces {
			if used[i] {
				continue
			}
			if !touches(i, first(i)) {
				start = i
				break
			}
			if !touches(i, last(i)) {
				start, rev = i, true
				break
			}
		}
		if start < 0 {
			for i := range pieces {
				if !used[i] {
					start = i
					break
				}
			}
		}
		if start < 0 {
			return chains
		}
		used[start] = true
		chain := []pieceRef{{start, rev}}
		end := func(r pieceRef) xy {
			if r.reversed {
				return first(r.index)
			}
			return last(r.index)
		}
		begin := func(r pieceRef) xy {
			if r.reversed {
				return last(r.index)
			}
			return first(r.index)
		}
		for {
			j, atStart := find(end(chain[len(chain)-1]))
			if j < 0 {
				break
			}
			used[j] = true
			chain = append(chain, pieceRef{j, !atStart})
		}
		for {
			j, atStart := find(begin(chain[0]))
			if j < 0 {
				break
			}
			used[j] = true
			chain = append([]pieceRef{{j, atStart}}, chain...)
		}
		chains = append(chains, chain)
	}
}

// centreline is a street's chains with a single measure (metres) along them.
type centreline struct {
	chains  [][]xy
	offsets []float64
	length  float64
}

func newCentreline(chains [][]xy) centreline {
	c := centreline{chains: chains}
	for _, ch := range chains {
		c.offsets = append(c.offsets, c.length)
		for k := 1; k < len(ch); k++ {
			c.length += dist(ch[k-1], ch[k])
		}
	}
	return c
}

// locate returns the measure of the centreline point nearest p, that point
// and the distance; the first nearest wins ties.
func (c centreline) locate(p xy) (measure float64, foot xy, d float64) {
	d = math.Inf(1)
	for ci, ch := range c.chains {
		s := c.offsets[ci]
		if len(ch) == 1 && dist(p, ch[0]) < d {
			measure, foot, d = s, ch[0], dist(p, ch[0])
		}
		for k := 1; k < len(ch); k++ {
			l := dist(ch[k-1], ch[k])
			t, f, dd := closestOnSegment(p, ch[k-1], ch[k])
			if dd < d {
				measure, foot, d = s+t*l, f, dd
			}
			s += l
		}
	}
	return measure, foot, d
}

// slice returns the parts of the centreline between measures from and to.
func (c centreline) slice(from, to float64) [][]xy {
	var parts [][]xy
	for ci, ch := range c.chains {
		s := c.offsets[ci]
		var part []xy
		for k := 1; k < len(ch); k++ {
			a, b := ch[k-1], ch[k]
			l := dist(a, b)
			lo, hi := math.Max(from, s), math.Min(to, s+l)
			if hi >= lo && l > 0 {
				pa := lerp(a, b, (lo-s)/l)
				pb := lerp(a, b, (hi-s)/l)
				if len(part) == 0 || part[len(part)-1] != pa {
					part = append(part, pa)
				}
				part = append(part, pb)
			}
			s += l
		}
		if len(part) > 0 {
			parts = append(parts, part)
		}
	}
	return parts
}

func lerp(a, b xy, t float64) xy { return xy{a.X + t*(b.X-a.X), a.Y + t*(b.Y-a.Y)} }

// capsule is the set of points within the outline radius of segment ab
// (a point when a == b), belonging to one label (segment).
type capsule struct {
	a, b  xy
	label int
}

type outlineParams struct {
	cellM     float64 // raster cell size
	radiusM   float64 // buffer radius around centrelines and home connectors
	simplifyM float64 // Douglas–Peucker tolerance
}

// defaultOutline: a 16 m buffer merges the connectors of neighbouring lots
// (frontages are ~12–15 m) into one band; 2 m cells and 1.5 m simplification
// keep the ring small without following individual lots.
var defaultOutline = outlineParams{cellM: 2, radiusM: 16, simplifyM: 1.5}

var errOutline = errors.New("site: outline")

// outlines rasterises the capsules: each cell within radius of any capsule
// takes the label of the nearest one (the earlier capsule on a tie), so
// neighbouring segments never overlap. Each label's largest 4-connected
// region, with holes filled, is traced to a simple counter-clockwise ring in
// metres and simplified. Labels without cells get nil.
func outlines(caps []capsule, nLabels int, prm outlineParams) ([][]xy, error) {
	if len(caps) == 0 {
		return make([][]xy, nLabels), nil
	}
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, c := range caps {
		minX, maxX = math.Min(minX, math.Min(c.a.X, c.b.X)), math.Max(maxX, math.Max(c.a.X, c.b.X))
		minY, maxY = math.Min(minY, math.Min(c.a.Y, c.b.Y)), math.Max(maxY, math.Max(c.a.Y, c.b.Y))
	}
	cell, r := prm.cellM, prm.radiusM
	pad := r + 2*cell
	x0, y0 := minX-pad, minY-pad
	w := int(math.Ceil((maxX + pad - x0) / cell))
	h := int(math.Ceil((maxY + pad - y0) / cell))
	if w <= 0 || h <= 0 || w*h > 50_000_000 {
		return nil, fmt.Errorf("%w: grid %d x %d cells is out of range", errOutline, w, h)
	}
	best := make([]float64, w*h)
	label := make([]int32, w*h)
	for k := range best {
		best[k], label[k] = math.Inf(1), -1
	}
	clampI := func(v, hi int) int { return max(0, min(v, hi)) }
	for _, c := range caps {
		i0 := clampI(int(math.Floor((math.Min(c.a.X, c.b.X)-r-x0)/cell)), w-1)
		i1 := clampI(int(math.Floor((math.Max(c.a.X, c.b.X)+r-x0)/cell)), w-1)
		j0 := clampI(int(math.Floor((math.Min(c.a.Y, c.b.Y)-r-y0)/cell)), h-1)
		j1 := clampI(int(math.Floor((math.Max(c.a.Y, c.b.Y)+r-y0)/cell)), h-1)
		for j := j0; j <= j1; j++ {
			cy := y0 + (float64(j)+0.5)*cell
			for i := i0; i <= i1; i++ {
				_, _, d := closestOnSegment(xy{x0 + (float64(i)+0.5)*cell, cy}, c.a, c.b)
				if k := j*w + i; d <= r && d < best[k] {
					best[k], label[k] = d, int32(c.label) //nolint:gosec // label < nLabels
				}
			}
		}
	}
	type box struct{ i0, j0, i1, j1, n int }
	boxes := make([]box, nLabels)
	for l := range boxes {
		boxes[l] = box{i0: w, j0: h, i1: -1, j1: -1}
	}
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			if l := label[j*w+i]; l >= 0 {
				b := &boxes[l]
				b.i0, b.i1, b.j0, b.j1, b.n = min(b.i0, i), max(b.i1, i), min(b.j0, j), max(b.j1, j), b.n+1
			}
		}
	}
	rings := make([][]xy, nLabels)
	for l, b := range boxes {
		if b.n == 0 {
			continue
		}
		// Window with a one-cell empty margin on every side.
		bw, bh := b.i1-b.i0+3, b.j1-b.j0+3
		in := make([]bool, bw*bh)
		for j := b.j0; j <= b.j1; j++ {
			for i := b.i0; i <= b.i1; i++ {
				in[(j-b.j0+1)*bw+(i-b.i0+1)] = label[j*w+i] == int32(l) //nolint:gosec // l < nLabels
			}
		}
		keep := largestComponent(in, bw, bh)
		fillHoles(keep, bw, bh)
		ring, err := traceRing(keep, bw, bh)
		if err != nil {
			return nil, fmt.Errorf("label %d: %w", l, err)
		}
		ox, oy := x0+float64(b.i0-1)*cell, y0+float64(b.j0-1)*cell
		for k := range ring {
			ring[k] = xy{ox + ring[k].X*cell, oy + ring[k].Y*cell}
		}
		rings[l] = simplifyRing(ring, prm.simplifyM)
	}
	return rings, nil
}

var neighbours4 = [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}

// largestComponent keeps the largest 4-connected true region of in (the
// first in raster order on a tie).
func largestComponent(in []bool, w, h int) []bool {
	comp := make([]int32, len(in))
	var bestID, id int32
	bestN := 0
	queue := make([]int, 0, 64)
	for start := range in {
		if !in[start] || comp[start] != 0 {
			continue
		}
		id++
		n := 0
		comp[start] = id
		queue = append(queue[:0], start)
		for len(queue) > 0 {
			k := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			n++
			i, j := k%w, k/w
			for _, d := range neighbours4 {
				ni, nj := i+d[0], j+d[1]
				if ni < 0 || nj < 0 || ni >= w || nj >= h {
					continue
				}
				if nk := nj*w + ni; in[nk] && comp[nk] == 0 {
					comp[nk] = id
					queue = append(queue, nk)
				}
			}
		}
		if n > bestN {
			bestN, bestID = n, id
		}
	}
	keep := make([]bool, len(in))
	for k := range comp {
		keep[k] = comp[k] == bestID && bestID != 0
	}
	return keep
}

// fillHoles sets every false cell not 4-connected to the window border.
func fillHoles(keep []bool, w, h int) {
	outside := make([]bool, len(keep))
	var queue []int
	push := func(k int) {
		if !keep[k] && !outside[k] {
			outside[k] = true
			queue = append(queue, k)
		}
	}
	for i := 0; i < w; i++ {
		push(i)
		push((h-1)*w + i)
	}
	for j := 0; j < h; j++ {
		push(j * w)
		push(j*w + w - 1)
	}
	for len(queue) > 0 {
		k := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j := k%w, k/w
		for _, d := range neighbours4 {
			if ni, nj := i+d[0], j+d[1]; ni >= 0 && nj >= 0 && ni < w && nj < h {
				push(nj*w + ni)
			}
		}
	}
	for k := range keep {
		if !outside[k] {
			keep[k] = true
		}
	}
}

// traceRing walks the boundary of a single hole-free 4-connected region
// (region on the left, so counter-clockwise with y up) and returns its corner
// vertices in cell units. A region that is 4-connected and hole-free has no
// diagonal-only pinch, so every boundary vertex has one outgoing edge.
func traceRing(keep []bool, w, h int) ([]xy, error) {
	vw := w + 1
	next := make([]int32, vw*(h+1))
	for k := range next {
		next[k] = -1
	}
	edges := 0
	var saddle bool
	set := func(from, to int) {
		if next[from] >= 0 {
			saddle = true
		}
		next[from] = int32(to) //nolint:gosec // grid index
		edges++
	}
	at := func(i, j int) bool { return i >= 0 && j >= 0 && i < w && j < h && keep[j*w+i] }
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			if !keep[j*w+i] {
				continue
			}
			if !at(i, j-1) {
				set(j*vw+i, j*vw+i+1)
			}
			if !at(i+1, j) {
				set(j*vw+i+1, (j+1)*vw+i+1)
			}
			if !at(i, j+1) {
				set((j+1)*vw+i+1, (j+1)*vw+i)
			}
			if !at(i-1, j) {
				set((j+1)*vw+i, j*vw+i)
			}
		}
	}
	if saddle {
		return nil, fmt.Errorf("%w: boundary vertex with two outgoing edges", errOutline)
	}
	start := -1
	for v, n := range next {
		if n >= 0 {
			start = v
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("%w: empty region", errOutline)
	}
	var verts []xy
	v, steps := start, 0
	for {
		verts = append(verts, xy{float64(v % vw), float64(v / vw)})
		v = int(next[v])
		steps++
		if v == start || v < 0 || steps > edges {
			break
		}
	}
	if v != start || steps != edges {
		return nil, fmt.Errorf("%w: boundary is not a single loop (%d of %d edges)", errOutline, steps, edges)
	}
	// Keep only corners.
	n := len(verts)
	var ring []xy
	for k := range verts {
		if cross(verts[(k+n-1)%n], verts[k], verts[(k+1)%n]) != 0 {
			ring = append(ring, verts[k])
		}
	}
	return ring, nil
}

// simplifyRing applies Douglas–Peucker to a closed ring (no repeated closing
// vertex) and falls back to the input if the result is not a simple ring.
func simplifyRing(ring []xy, tol float64) []xy {
	n := len(ring)
	if n < 5 || tol <= 0 {
		return ring
	}
	far, farD := 0, -1.0
	for k, p := range ring {
		if d := dist(ring[0], p); d > farD {
			far, farD = k, d
		}
	}
	a := douglasPeucker(ring[:far+1], tol)
	b := douglasPeucker(append(append([]xy{}, ring[far:]...), ring[0]), tol)
	out := append(append([]xy{}, a[:len(a)-1]...), b[:len(b)-1]...)
	if !ringSimple(out) {
		return ring
	}
	return out
}

func douglasPeucker(pts []xy, tol float64) []xy {
	if len(pts) < 3 {
		return pts
	}
	keep := make([]bool, len(pts))
	keep[0], keep[len(pts)-1] = true, true
	stack := [][2]int{{0, len(pts) - 1}}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		idx, maxD := -1, 0.0
		for k := s[0] + 1; k < s[1]; k++ {
			if _, _, d := closestOnSegment(pts[k], pts[s[0]], pts[s[1]]); d > maxD {
				idx, maxD = k, d
			}
		}
		if idx >= 0 && maxD > tol {
			keep[idx] = true
			stack = append(stack, [2]int{s[0], idx}, [2]int{idx, s[1]})
		}
	}
	var out []xy
	for k, p := range pts {
		if keep[k] {
			out = append(out, p)
		}
	}
	return out
}

func signedArea(ring []xy) float64 {
	var a float64
	for k := range ring {
		p, q := ring[k], ring[(k+1)%len(ring)]
		a += p.X*q.Y - q.X*p.Y
	}
	return a / 2
}

// ringSimple reports whether a closed ring (no repeated closing vertex) has
// at least three vertices, non-zero area, no repeated or folded-back
// vertices, and no two non-adjacent edges that touch.
func ringSimple(ring []xy) bool {
	n := len(ring)
	if n < 3 || signedArea(ring) == 0 {
		return false
	}
	for k := range ring {
		prev, cur, nxt := ring[(k+n-1)%n], ring[k], ring[(k+1)%n]
		if cur == nxt {
			return false
		}
		if cross(prev, cur, nxt) == 0 && (cur.X-prev.X)*(nxt.X-cur.X)+(cur.Y-prev.Y)*(nxt.Y-cur.Y) < 0 {
			return false
		}
	}
	for i := 0; i < n; i++ {
		a1, a2 := ring[i], ring[(i+1)%n]
		for j := i + 2; j < n; j++ {
			if i == 0 && j == n-1 {
				continue // adjacent through the closing vertex
			}
			if segmentsTouch(a1, a2, ring[j], ring[(j+1)%n]) {
				return false
			}
		}
	}
	return true
}

func segmentsTouch(p1, p2, q1, q2 xy) bool {
	d1, d2 := cross(q1, q2, p1), cross(q1, q2, p2)
	d3, d4 := cross(p1, p2, q1), cross(p1, p2, q2)
	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) && ((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true
	}
	on := func(a, b, p xy) bool {
		return p.X >= math.Min(a.X, b.X) && p.X <= math.Max(a.X, b.X) && p.Y >= math.Min(a.Y, b.Y) && p.Y <= math.Max(a.Y, b.Y)
	}
	return (d1 == 0 && on(q1, q2, p1)) || (d2 == 0 && on(q1, q2, p2)) || (d3 == 0 && on(p1, p2, q1)) || (d4 == 0 && on(p1, p2, q2))
}

// pointInRing is the even-odd rule.
func pointInRing(p xy, ring []xy) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		a, b := ring[i], ring[j]
		if (a.Y > p.Y) != (b.Y > p.Y) && p.X < (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y)+a.X {
			in = !in
		}
	}
	return in
}
