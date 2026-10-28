package geo

import "container/list"

// lru is a fixed-capacity least-recently-used cache. Not safe for concurrent
// use; Router guards it with a mutex.
type lru[K comparable, V any] struct {
	cap   int
	ll    *list.List
	items map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
}

func newLRU[K comparable, V any](capacity int) *lru[K, V] {
	return &lru[K, V]{cap: capacity, ll: list.New(), items: make(map[K]*list.Element, capacity)}
}

func (c *lru[K, V]) Get(k K) (V, bool) {
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruEntry[K, V]).val, true
	}
	var zero V
	return zero, false
}

func (c *lru[K, V]) Put(k K, v V) {
	if c.cap <= 0 {
		return
	}
	if el, ok := c.items[k]; ok {
		el.Value.(*lruEntry[K, V]).val = v
		c.ll.MoveToFront(el)
		return
	}
	c.items[k] = c.ll.PushFront(&lruEntry[K, V]{key: k, val: v})
	if c.ll.Len() > c.cap {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*lruEntry[K, V]).key)
	}
}

func (c *lru[K, V]) Len() int { return c.ll.Len() }
