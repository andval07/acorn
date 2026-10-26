package acorn

import (
	"container/list"
	"sync"
)

// cache keeps recently used decoded nodes, so hot pages (the root and
// the upper levels) are not read and decoded on every lookup. Readers
// share it, so it has its own lock.
type cache struct {
	mu    sync.Mutex
	limit int
	order *list.List // front = most recently used
	items map[uint64]*list.Element
}

func newCache(limit int) *cache {
	return &cache{limit: limit, order: list.New(), items: map[uint64]*list.Element{}}
}

func (c *cache) get(id uint64) (*node, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*node), true
}

func (c *cache) put(n *node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[n.id]; ok {
		e.Value = n
		c.order.MoveToFront(e)
		return
	}
	c.items[n.id] = c.order.PushFront(n)
	if c.order.Len() > c.limit {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*node).id)
	}
}
