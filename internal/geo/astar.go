package geo

import (
	"container/heap"
	"math"
)

// Path is a route through the network.
type Path struct {
	Nodes   []NodeID
	Meters  float64
	Seconds float64
}

// ShortestPath returns the minimum travel-time path from src to dst using A*.
//
// The heuristic is straight-line (great-circle) distance to dst divided by the
// network's maximum road speed. Every edge is at least as long as the
// straight line between its endpoints and no edge is faster than the maximum
// speed, so the heuristic never overestimates the remaining travel time
// (admissible). It also satisfies the triangle inequality per edge
// (consistent), so each node is settled at most once.
func (n *Network) ShortestPath(src, dst NodeID) (Path, error) {
	goal := n.nodes[dst]
	inv := 1 / n.maxSpeedMps
	return n.search(src, dst, func(id NodeID) float64 {
		return Haversine(n.nodes[id], goal) * inv
	})
}

// Dijkstra returns the minimum travel-time path with no heuristic. It exists
// as the reference implementation for testing A*.
func (n *Network) Dijkstra(src, dst NodeID) (Path, error) {
	return n.search(src, dst, func(NodeID) float64 { return 0 })
}

func (n *Network) search(src, dst NodeID, h func(NodeID) float64) (Path, error) {
	if src == dst {
		return Path{Nodes: []NodeID{src}}, nil
	}
	if n.component != nil && !n.SameComponent(src, dst) {
		return Path{}, ErrUnreachable
	}
	size := len(n.nodes)
	g := make([]float64, size)
	for i := range g {
		g[i] = math.Inf(1)
	}
	prev := make([]NodeID, size)
	closed := make([]bool, size)
	g[src] = 0
	prev[src] = -1
	pq := &nodeHeap{{id: src, f: h(src)}}
	for pq.Len() > 0 {
		cur := heap.Pop(pq).(heapItem)
		u := cur.id
		if closed[u] {
			continue
		}
		if u == dst {
			break
		}
		closed[u] = true
		for _, e := range n.adj[u] {
			if closed[e.To] {
				continue
			}
			ng := g[u] + e.Seconds
			if ng < g[e.To] {
				g[e.To] = ng
				prev[e.To] = u
				heap.Push(pq, heapItem{id: e.To, f: ng + h(e.To)})
			}
		}
	}
	if math.IsInf(g[dst], 1) {
		return Path{}, ErrUnreachable
	}
	var nodes []NodeID
	for v := dst; v != -1; v = prev[v] {
		nodes = append(nodes, v)
	}
	for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	}
	meters := 0.0
	for i := 1; i < len(nodes); i++ {
		for _, e := range n.adj[nodes[i-1]] {
			if e.To == nodes[i] {
				meters += e.Meters
				break
			}
		}
	}
	return Path{Nodes: nodes, Meters: meters, Seconds: g[dst]}, nil
}

type heapItem struct {
	id NodeID
	f  float64
}

type nodeHeap []heapItem

func (h nodeHeap) Len() int { return len(h) }
func (h nodeHeap) Less(i, j int) bool {
	if h[i].f != h[j].f {
		return h[i].f < h[j].f
	}
	return h[i].id < h[j].id
}
func (h nodeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x any)   { *h = append(*h, x.(heapItem)) }
func (h *nodeHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}
