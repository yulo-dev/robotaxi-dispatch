package geo

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
)

// NodeID identifies a node in a Network.
type NodeID int32

// Edge is a directed traversable edge in the adjacency list.
type Edge struct {
	To      NodeID
	Meters  float64
	Seconds float64
}

// GridConfig controls synthetic grid generation.
type GridConfig struct {
	BBox        BBox
	SpacingM    float64   // approximate distance between adjacent intersections
	SpeedsKmh   []float64 // road classes; each street gets one class
	ClassWeight []float64 // relative frequency of each class (same length as SpeedsKmh)
	BlockedFrac float64   // fraction of edges removed (closures)
	Seed        int64
}

// DefaultGridConfig returns the default San Francisco grid.
func DefaultGridConfig() GridConfig {
	return GridConfig{
		BBox:        SanFrancisco,
		SpacingM:    200,
		SpeedsKmh:   []float64{25, 40, 55},
		ClassWeight: []float64{0.6, 0.3, 0.1},
		BlockedFrac: 0.05,
		Seed:        1,
	}
}

// Network is an immutable road graph. All methods are safe for concurrent use.
type Network struct {
	nodes       []Location
	adj         [][]Edge
	rows, cols  int
	bbox        BBox
	maxSpeedMps float64
	component   []int32 // connected-component id per node
	mainComp    int32   // id of the largest component
	mainNodes   []NodeID
	blocked     map[[2]NodeID]bool
}

// ErrUnreachable is returned when no path exists between two nodes.
var ErrUnreachable = errors.New("geo: destination unreachable")

// NewGrid builds a grid road network from cfg. Generation is deterministic
// for a given cfg.
func NewGrid(cfg GridConfig) (*Network, error) {
	if cfg.SpacingM <= 0 {
		return nil, fmt.Errorf("geo: spacing must be positive")
	}
	if len(cfg.SpeedsKmh) == 0 || len(cfg.SpeedsKmh) != len(cfg.ClassWeight) {
		return nil, fmt.Errorf("geo: speeds and class weights must be non-empty and equal length")
	}
	if cfg.BlockedFrac < 0 || cfg.BlockedFrac >= 1 {
		return nil, fmt.Errorf("geo: blocked fraction must be in [0,1)")
	}
	b := cfg.BBox
	midLat := (b.MinLat + b.MaxLat) / 2
	heightM := Haversine(Location{b.MinLat, b.MinLng}, Location{b.MaxLat, b.MinLng})
	widthM := Haversine(Location{midLat, b.MinLng}, Location{midLat, b.MaxLng})
	rows := int(math.Round(heightM/cfg.SpacingM)) + 1
	cols := int(math.Round(widthM/cfg.SpacingM)) + 1
	if rows < 2 || cols < 2 {
		return nil, fmt.Errorf("geo: bounding box too small for spacing")
	}

	rng := rand.New(rand.NewSource(cfg.Seed))
	pickClass := func() float64 {
		total := 0.0
		for _, w := range cfg.ClassWeight {
			total += w
		}
		x := rng.Float64() * total
		for i, w := range cfg.ClassWeight {
			if x < w {
				return cfg.SpeedsKmh[i]
			}
			x -= w
		}
		return cfg.SpeedsKmh[len(cfg.SpeedsKmh)-1]
	}
	// One class per street: horizontal streets (rows) then vertical (cols).
	rowSpeed := make([]float64, rows)
	for i := range rowSpeed {
		rowSpeed[i] = pickClass()
	}
	colSpeed := make([]float64, cols)
	for i := range colSpeed {
		colSpeed[i] = pickClass()
	}

	n := &Network{
		nodes:   make([]Location, rows*cols),
		adj:     make([][]Edge, rows*cols),
		rows:    rows,
		cols:    cols,
		bbox:    b,
		blocked: map[[2]NodeID]bool{},
	}
	for r := 0; r < rows; r++ {
		lat := b.MinLat + (b.MaxLat-b.MinLat)*float64(r)/float64(rows-1)
		for c := 0; c < cols; c++ {
			lng := b.MinLng + (b.MaxLng-b.MinLng)*float64(c)/float64(cols-1)
			n.nodes[r*cols+c] = Location{Lat: lat, Lng: lng}
		}
	}
	for _, s := range cfg.SpeedsKmh {
		n.maxSpeedMps = math.Max(n.maxSpeedMps, KmhToMps(s))
	}

	addEdge := func(a, bID NodeID, kmh float64) {
		if rng.Float64() < cfg.BlockedFrac {
			n.blocked[edgeKey(a, bID)] = true
			return
		}
		m := Haversine(n.nodes[a], n.nodes[bID])
		s := m / KmhToMps(kmh)
		n.adj[a] = append(n.adj[a], Edge{To: bID, Meters: m, Seconds: s})
		n.adj[bID] = append(n.adj[bID], Edge{To: a, Meters: m, Seconds: s})
	}
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			id := NodeID(r*cols + c)
			if c+1 < cols {
				addEdge(id, id+1, rowSpeed[r])
			}
			if r+1 < rows {
				addEdge(id, id+NodeID(cols), colSpeed[c])
			}
		}
	}
	n.computeComponents()
	return n, nil
}

func edgeKey(a, b NodeID) [2]NodeID {
	if a > b {
		a, b = b, a
	}
	return [2]NodeID{a, b}
}

func (n *Network) computeComponents() {
	n.component = make([]int32, len(n.nodes))
	for i := range n.component {
		n.component[i] = -1
	}
	var sizes []int
	stack := make([]NodeID, 0, 64)
	for start := range n.nodes {
		if n.component[start] >= 0 {
			continue
		}
		id := int32(len(sizes))
		size := 0
		stack = append(stack[:0], NodeID(start))
		n.component[start] = id
		for len(stack) > 0 {
			u := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			size++
			for _, e := range n.adj[u] {
				if n.component[e.To] < 0 {
					n.component[e.To] = id
					stack = append(stack, e.To)
				}
			}
		}
		sizes = append(sizes, size)
	}
	best := 0
	for i, s := range sizes {
		if s > sizes[best] {
			best = i
		}
	}
	n.mainComp = int32(best)
	for i, c := range n.component {
		if c == n.mainComp {
			n.mainNodes = append(n.mainNodes, NodeID(i))
		}
	}
}

// NumNodes returns the number of nodes.
func (n *Network) NumNodes() int { return len(n.nodes) }

// Dims returns grid rows and columns.
func (n *Network) Dims() (rows, cols int) { return n.rows, n.cols }

// Node returns the location of node id.
func (n *Network) Node(id NodeID) Location { return n.nodes[id] }

// Neighbors returns the traversable edges out of id. The slice must not be modified.
func (n *Network) Neighbors(id NodeID) []Edge { return n.adj[id] }

// MaxSpeedMps is the fastest road class in m/s.
func (n *Network) MaxSpeedMps() float64 { return n.maxSpeedMps }

// IsBlocked reports whether the grid edge between a and b was closed at generation.
func (n *Network) IsBlocked(a, b NodeID) bool { return n.blocked[edgeKey(a, b)] }

// HasEdge reports whether a traversable edge a->b exists.
func (n *Network) HasEdge(a, b NodeID) bool {
	for _, e := range n.adj[a] {
		if e.To == b {
			return true
		}
	}
	return false
}

// InMainComponent reports whether id belongs to the largest connected component.
func (n *Network) InMainComponent(id NodeID) bool { return n.component[id] == n.mainComp }

// MainNodes returns the nodes of the largest connected component. Read-only.
func (n *Network) MainNodes() []NodeID { return n.mainNodes }

// SameComponent reports whether a and b are connected.
func (n *Network) SameComponent(a, b NodeID) bool { return n.component[a] == n.component[b] }

// NearestNode returns the closest node to loc that lies in the main connected
// component, so that any two snapped locations are mutually reachable.
// Locations outside the bounding box are clamped first.
func (n *Network) NearestNode(loc Location) NodeID {
	b := n.bbox
	lat := math.Min(math.Max(loc.Lat, b.MinLat), b.MaxLat)
	lng := math.Min(math.Max(loc.Lng, b.MinLng), b.MaxLng)
	r0 := int(math.Round((lat - b.MinLat) / (b.MaxLat - b.MinLat) * float64(n.rows-1)))
	c0 := int(math.Round((lng - b.MinLng) / (b.MaxLng - b.MinLng) * float64(n.cols-1)))
	target := Location{lat, lng}
	// Search square rings of increasing radius. Every node on ring rad is at
	// least (rad-0.5)*minCell meters away, so stop once that bound exceeds the
	// best distance found.
	best, bestD := NodeID(-1), math.Inf(1)
	maxR := n.rows + n.cols
	minCell := n.minCellM()
	for rad := 0; rad <= maxR; rad++ {
		if best >= 0 && (float64(rad)-0.5)*minCell > bestD {
			break
		}
		for r := r0 - rad; r <= r0+rad; r++ {
			if r < 0 || r >= n.rows {
				continue
			}
			for c := c0 - rad; c <= c0+rad; c++ {
				if c < 0 || c >= n.cols {
					continue
				}
				if abs(r-r0) != rad && abs(c-c0) != rad {
					continue // interior already visited
				}
				id := NodeID(r*n.cols + c)
				if !n.InMainComponent(id) {
					continue
				}
				if d := Haversine(target, n.nodes[id]); d < bestD {
					best, bestD = id, d
				}
			}
		}
	}
	return best
}

// minCellM is a lower bound on the spacing between adjacent grid lines.
func (n *Network) minCellM() float64 {
	if n.rows < 2 || n.cols < 2 {
		return 0
	}
	b := n.bbox
	h := Haversine(Location{b.MinLat, b.MinLng}, Location{b.MaxLat, b.MinLng}) / float64(n.rows-1)
	// Longitude spacing shrinks toward the pole; use the edge nearest the pole.
	poleLat := b.MaxLat
	if math.Abs(b.MinLat) > math.Abs(b.MaxLat) {
		poleLat = b.MinLat
	}
	w := Haversine(Location{poleLat, b.MinLng}, Location{poleLat, b.MaxLng}) / float64(n.cols-1)
	return math.Min(h, w)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// newNetworkFromEdges builds a small custom network; used by tests.
func newNetworkFromEdges(nodes []Location, edges [][2]NodeID, kmh float64) *Network {
	n := &Network{
		nodes:       nodes,
		adj:         make([][]Edge, len(nodes)),
		rows:        1,
		cols:        len(nodes),
		maxSpeedMps: KmhToMps(kmh),
		blocked:     map[[2]NodeID]bool{},
	}
	for _, e := range edges {
		m := Haversine(nodes[e[0]], nodes[e[1]])
		s := m / KmhToMps(kmh)
		n.adj[e[0]] = append(n.adj[e[0]], Edge{To: e[1], Meters: m, Seconds: s})
		n.adj[e[1]] = append(n.adj[e[1]], Edge{To: e[0], Meters: m, Seconds: s})
	}
	n.computeComponents()
	return n
}
