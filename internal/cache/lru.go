// Package cache is §8.1's machinery (SPEC.md): a least-recently-used map
// bounded by bytes, and a group that shares one upstream call among
// concurrent requests for the same key. What is cached, and for how long,
// is the HTTP layer's.
package cache

import (
	"container/list"
	"sync"
	"time"
)

// Entry is one stored value.
type Entry[V any] struct {
	Value  V
	Stored time.Time
	Fresh  bool // within its time to live at the lookup
}

// LRU maps keys to values, at most max bytes of them by their callers'
// sizes, dropping the least recently used first. It is safe for
// concurrent use.
type LRU[V any] struct {
	mu    sync.Mutex
	max   int64
	bytes int64
	order *list.List // of *item[V], most recently used first
	items map[string]*list.Element
}

type item[V any] struct {
	key            string
	value          V
	size           int64
	stored         time.Time
	fresh, expires time.Time // fresh until, kept until
}

// NewLRU returns an empty LRU of at most max bytes.
func NewLRU[V any](max int64) *LRU[V] {
	return &LRU[V]{max: max, order: list.New(), items: map[string]*list.Element{}}
}

// Put stores v under key, replacing what was there: size bytes, stored at
// now, fresh for ttl and kept for keep (at least ttl). A value larger than
// the whole LRU is not stored, and what was under key is dropped.
func (c *LRU[V]) Put(key string, v V, size int64, now time.Time, ttl, keep time.Duration) {
	keep = max(keep, ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.remove(el)
	}
	if size > c.max {
		return
	}
	it := &item[V]{key: key, value: v, size: size, stored: now, fresh: now.Add(ttl), expires: now.Add(keep)}
	c.items[key] = c.order.PushFront(it)
	c.bytes += size
	for c.bytes > c.max {
		c.remove(c.order.Back())
	}
}

// Get returns the entry under key at now. An entry past its keep time is
// dropped and not returned.
func (c *LRU[V]) Get(key string, now time.Time) (Entry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Entry[V]{}, false
	}
	it := el.Value.(*item[V])
	if !now.Before(it.expires) {
		c.remove(el)
		return Entry[V]{}, false
	}
	c.order.MoveToFront(el)
	return Entry[V]{Value: it.value, Stored: it.stored, Fresh: now.Before(it.fresh)}, true
}

// Len returns the number of entries and their bytes.
func (c *LRU[V]) Len() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.bytes
}

func (c *LRU[V]) remove(el *list.Element) {
	it := c.order.Remove(el).(*item[V])
	delete(c.items, it.key)
	c.bytes -= it.size
}
