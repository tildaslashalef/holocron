package cache

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// PanicError is a panic recovered in a shared call: a bug (internal).
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Group runs at most one call per key at a time; callers with the same key
// while it runs wait for its result. The zero Group is ready to use.
type Group[V any] struct {
	mu    sync.Mutex
	calls map[string]*call[V]
}

type call[V any] struct {
	done    chan struct{}
	value   V
	err     error
	waiting int
	cancel  context.CancelFunc
}

// Do returns fn's result for key, starting fn unless a call for key is
// running already. fn runs in its own goroutine under a context detached
// from ctx's cancellation (its values kept), which ends when fn returns or
// when every caller waiting for it has left. Do returns ctx's error if ctx
// ends first. A panic in fn is a *PanicError.
func (g *Group[V]) Do(ctx context.Context, key string, fn func(context.Context) (V, error)) (V, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*call[V]{}
	}
	c, ok := g.calls[key]
	if !ok {
		cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c = &call[V]{done: make(chan struct{}), cancel: cancel}
		g.calls[key] = c
		go g.run(cctx, key, c, fn)
	}
	c.waiting++
	g.mu.Unlock()

	select {
	case <-c.done:
		return c.value, c.err
	case <-ctx.Done():
	}
	g.mu.Lock()
	c.waiting--
	if c.waiting == 0 {
		// Nobody wants the result any more; a later caller starts afresh.
		c.cancel()
		g.forget(key, c)
	}
	g.mu.Unlock()
	var zero V
	return zero, ctx.Err()
}

func (g *Group[V]) run(ctx context.Context, key string, c *call[V], fn func(context.Context) (V, error)) {
	defer func() {
		// The goroutine is beyond any handler's recover: a panic here must
		// not end the process.
		if v := recover(); v != nil {
			c.err = &PanicError{Value: v, Stack: debug.Stack()}
		}
		c.cancel()
		g.mu.Lock()
		g.forget(key, c)
		g.mu.Unlock()
		close(c.done)
	}()
	c.value, c.err = fn(ctx)
}

// Waiting returns how many callers wait for key's running call: 0 when
// none runs.
func (g *Group[V]) Waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c.waiting
	}
	return 0
}

// forget removes c from the running calls if it is still there; g.mu is
// held.
func (g *Group[V]) forget(key string, c *call[V]) {
	if g.calls[key] == c {
		delete(g.calls, key)
	}
}
