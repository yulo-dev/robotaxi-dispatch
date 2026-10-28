package geo

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"testing"
)

func testNet(t testing.TB) *Network {
	t.Helper()
	n, err := NewGrid(DefaultGridConfig())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHaversineKnownDistance(t *testing.T) {
	// One degree of latitude is ~111.2 km.
	d := Haversine(Location{37, -122}, Location{38, -122})
	if math.Abs(d-111195) > 100 {
		t.Fatalf("got %.0f m, want ~111195 m", d)
	}
	if Haversine(Location{37.7, -122.4}, Location{37.7, -122.4}) != 0 {
		t.Fatal("distance to self must be 0")
	}
}

func TestGridDeterministic(t *testing.T) {
	a, b := testNet(t), testNet(t)
	if a.NumNodes() != b.NumNodes() {
		t.Fatal("node count differs")
	}
	for i := 0; i < a.NumNodes(); i++ {
		ea, eb := a.Neighbors(NodeID(i)), b.Neighbors(NodeID(i))
		if len(ea) != len(eb) {
			t.Fatalf("node %d degree differs", i)
		}
		for j := range ea {
			if ea[j] != eb[j] {
				t.Fatalf("node %d edge %d differs", i, j)
			}
		}
	}
}

func TestGridBlockedFraction(t *testing.T) {
	n := testNet(t)
	rows, cols := n.Dims()
	total := rows*(cols-1) + cols*(rows-1)
	frac := float64(len(n.blocked)) / float64(total)
	if frac < 0.03 || frac > 0.07 {
		t.Fatalf("blocked fraction %.3f, want about 0.05", frac)
	}
	if len(n.MainNodes()) < n.NumNodes()*9/10 {
		t.Fatalf("main component too small: %d of %d", len(n.MainNodes()), n.NumNodes())
	}
}

func randomPairs(n *Network, count int, seed int64) [][2]NodeID {
	rng := rand.New(rand.NewSource(seed))
	main := n.MainNodes()
	out := make([][2]NodeID, count)
	for i := range out {
		out[i] = [2]NodeID{main[rng.Intn(len(main))], main[rng.Intn(len(main))]}
	}
	return out
}

func TestAStarEqualsDijkstra(t *testing.T) {
	n := testNet(t)
	for i, p := range randomPairs(n, 200, 42) {
		a, err := n.ShortestPath(p[0], p[1])
		if err != nil {
			t.Fatalf("pair %d: astar: %v", i, err)
		}
		d, err := n.Dijkstra(p[0], p[1])
		if err != nil {
			t.Fatalf("pair %d: dijkstra: %v", i, err)
		}
		if math.Abs(a.Seconds-d.Seconds) > 1e-6 {
			t.Fatalf("pair %d (%d->%d): astar %.6f s, dijkstra %.6f s", i, p[0], p[1], a.Seconds, d.Seconds)
		}
	}
}

func TestRouteAtLeastStraightLine(t *testing.T) {
	n := testNet(t)
	for _, p := range randomPairs(n, 200, 7) {
		path, err := n.ShortestPath(p[0], p[1])
		if err != nil {
			t.Fatal(err)
		}
		sl := Haversine(n.Node(p[0]), n.Node(p[1]))
		if path.Meters+1e-6 < sl {
			t.Fatalf("route %.1f m shorter than straight line %.1f m", path.Meters, sl)
		}
		if path.Seconds+1e-6 < sl/n.MaxSpeedMps() {
			t.Fatalf("route %.1f s faster than lower bound %.1f s", path.Seconds, sl/n.MaxSpeedMps())
		}
	}
}

func TestRouteNeverUsesBlockedEdges(t *testing.T) {
	n := testNet(t)
	if len(n.blocked) == 0 {
		t.Fatal("expected some blocked edges")
	}
	for _, p := range randomPairs(n, 200, 9) {
		path, err := n.ShortestPath(p[0], p[1])
		if err != nil {
			t.Fatal(err)
		}
		if path.Nodes[0] != p[0] || path.Nodes[len(path.Nodes)-1] != p[1] {
			t.Fatal("path endpoints wrong")
		}
		for i := 1; i < len(path.Nodes); i++ {
			a, b := path.Nodes[i-1], path.Nodes[i]
			if n.IsBlocked(a, b) {
				t.Fatalf("path uses blocked edge %d-%d", a, b)
			}
			if !n.HasEdge(a, b) {
				t.Fatalf("path uses non-edge %d-%d", a, b)
			}
		}
	}
}

func TestUnreachableReturnsError(t *testing.T) {
	nodes := []Location{{37.70, -122.50}, {37.70, -122.49}, {37.71, -122.49}}
	n := newNetworkFromEdges(nodes, [][2]NodeID{{0, 1}}, 40)
	if _, err := n.ShortestPath(0, 2); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable", err)
	}
	if _, err := n.Dijkstra(0, 2); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("dijkstra got %v, want ErrUnreachable", err)
	}
	if p, err := n.ShortestPath(0, 1); err != nil || len(p.Nodes) != 2 {
		t.Fatalf("reachable pair failed: %v", err)
	}
}

func TestUnreachableOnGrid(t *testing.T) {
	cfg := DefaultGridConfig()
	cfg.BlockedFrac = 0.45 // fragment the grid
	n, err := NewGrid(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var outside NodeID = -1
	for i := 0; i < n.NumNodes(); i++ {
		if !n.InMainComponent(NodeID(i)) {
			outside = NodeID(i)
			break
		}
	}
	if outside < 0 {
		t.Skip("grid happened to stay connected")
	}
	if _, err := n.ShortestPath(n.MainNodes()[0], outside); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable", err)
	}
}

func TestNearestNode(t *testing.T) {
	n := testNet(t)
	rng := rand.New(rand.NewSource(3))
	b := SanFrancisco
	for i := 0; i < 100; i++ {
		loc := Location{
			Lat: b.MinLat + rng.Float64()*(b.MaxLat-b.MinLat),
			Lng: b.MinLng + rng.Float64()*(b.MaxLng-b.MinLng),
		}
		got := n.NearestNode(loc)
		if !n.InMainComponent(got) {
			t.Fatal("nearest node not in main component")
		}
		// Brute force over main-component nodes.
		bestD := math.Inf(1)
		for _, id := range n.MainNodes() {
			bestD = math.Min(bestD, Haversine(loc, n.Node(id)))
		}
		if d := Haversine(loc, n.Node(got)); d > bestD+1e-6 {
			t.Fatalf("nearest node %.1f m away, brute force %.1f m", d, bestD)
		}
	}
	// Outside the box clamps instead of failing.
	if id := n.NearestNode(Location{0, 0}); id < 0 {
		t.Fatal("clamped lookup failed")
	}
}

func TestRouterCacheHitRate(t *testing.T) {
	n := testNet(t)
	r := NewRouter(n, 16)
	pairs := randomPairs(n, 4, 1)
	for round := 0; round < 5; round++ {
		for _, p := range pairs {
			if _, err := r.Route(p[0], p[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := r.Stats()
	if s.Misses != 4 || s.Hits != 16 {
		t.Fatalf("hits=%d misses=%d, want 16/4", s.Hits, s.Misses)
	}
	if math.Abs(s.HitRate()-0.8) > 1e-9 {
		t.Fatalf("hit rate %.3f", s.HitRate())
	}
}

func TestRouterConcurrent(t *testing.T) {
	n := testNet(t)
	r := NewRouter(n, 64)
	pairs := randomPairs(n, 32, 5)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, p := range pairs {
				if _, err := r.Route(p[0], p[1]); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestLRUEviction(t *testing.T) {
	c := newLRU[int, int](2)
	c.Put(1, 1)
	c.Put(2, 2)
	c.Get(1)
	c.Put(3, 3) // evicts 2
	if _, ok := c.Get(2); ok {
		t.Fatal("2 should be evicted")
	}
	if v, ok := c.Get(1); !ok || v != 1 {
		t.Fatal("1 should remain")
	}
	if c.Len() != 2 {
		t.Fatal("len")
	}
}

func BenchmarkAStar(b *testing.B) {
	n := testNet(b)
	pairs := randomPairs(n, 256, 11)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := pairs[i%len(pairs)]
		if _, err := n.ShortestPath(p[0], p[1]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDijkstra(b *testing.B) {
	n := testNet(b)
	pairs := randomPairs(n, 256, 11)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := pairs[i%len(pairs)]
		if _, err := n.Dijkstra(p[0], p[1]); err != nil {
			b.Fatal(err)
		}
	}
}
