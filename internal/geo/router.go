package geo

import (
	"sync"
	"time"
)

// RouteStats reports cache behaviour.
type RouteStats struct {
	Hits, Misses uint64
}

// HitRate returns hits / (hits + misses), or 0 when there were no lookups.
func (s RouteStats) HitRate() float64 {
	if s.Hits+s.Misses == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Hits+s.Misses)
}

// Router answers route and ETA queries over a Network with an LRU cache keyed
// by (from node, to node). It is safe for concurrent use.
type Router struct {
	net *Network

	mu    sync.Mutex
	cache *lru[[2]NodeID, Path]
	stats RouteStats

	// OnRoute, if set, is called with the duration of every uncached A* search.
	OnRoute func(time.Duration)
}

// NewRouter returns a Router with the given cache capacity (0 disables caching).
func NewRouter(n *Network, cacheSize int) *Router {
	return &Router{net: n, cache: newLRU[[2]NodeID, Path](cacheSize)}
}

// Network returns the underlying network.
func (r *Router) Network() *Network { return r.net }

// NearestNode snaps loc to the network.
func (r *Router) NearestNode(loc Location) NodeID { return r.net.NearestNode(loc) }

// Route returns the fastest path between two nodes.
func (r *Router) Route(from, to NodeID) (Path, error) {
	key := [2]NodeID{from, to}
	r.mu.Lock()
	if p, ok := r.cache.Get(key); ok {
		r.stats.Hits++
		r.mu.Unlock()
		return p, nil
	}
	r.stats.Misses++
	r.mu.Unlock()

	start := time.Now()
	p, err := r.net.ShortestPath(from, to)
	if r.OnRoute != nil {
		r.OnRoute(time.Since(start))
	}
	if err != nil {
		return Path{}, err
	}
	r.mu.Lock()
	r.cache.Put(key, p)
	r.mu.Unlock()
	return p, nil
}

// RouteLoc snaps both locations to the network and routes between them.
func (r *Router) RouteLoc(from, to Location) (Path, error) {
	return r.Route(r.net.NearestNode(from), r.net.NearestNode(to))
}

// ETA returns road-network travel time in seconds between two locations,
// measured between the nearest network nodes.
func (r *Router) ETA(from, to Location) (float64, error) {
	p, err := r.RouteLoc(from, to)
	if err != nil {
		return 0, err
	}
	return p.Seconds, nil
}

// Stats returns a snapshot of cache counters.
func (r *Router) Stats() RouteStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// StraightLineETA returns haversine distance divided by speedMps. It is the
// cost used by the "straight" strategies and needs no network.
func StraightLineETA(from, to Location, speedMps float64) float64 {
	return Haversine(from, to) / speedMps
}
