package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func TestLRUHitMissAndTTL(t *testing.T) {
	c := NewLRU[string](1000)
	if _, ok := c.Get("a", t0); ok {
		t.Fatal("hit on an empty LRU")
	}
	c.Put("a", "A", 10, t0, time.Minute, time.Hour)
	e, ok := c.Get("a", t0.Add(59*time.Second))
	if !ok || e.Value != "A" || !e.Fresh || !e.Stored.Equal(t0) {
		t.Errorf("fresh: %+v %v", e, ok)
	}
	e, ok = c.Get("a", t0.Add(time.Minute))
	if !ok || e.Fresh {
		t.Errorf("past its ttl: %+v %v, want kept and not fresh", e, ok)
	}
	if _, ok := c.Get("a", t0.Add(time.Hour)); ok {
		t.Error("returned past its keep time")
	}
	if n, b := c.Len(); n != 0 || b != 0 {
		t.Errorf("after expiry: %d entries, %d bytes", n, b)
	}

	// keep below ttl is ttl.
	c.Put("b", "B", 10, t0, time.Minute, 0)
	if e, ok := c.Get("b", t0.Add(30*time.Second)); !ok || !e.Fresh {
		t.Errorf("keep 0: %+v %v", e, ok)
	}
}

func TestLRUEvictsByBytes(t *testing.T) {
	c := NewLRU[int](100)
	for i := range 4 {
		c.Put(fmt.Sprint(i), i, 30, t0, time.Hour, time.Hour) // 0..3: 120 bytes, 0 evicted
	}
	if _, ok := c.Get("0", t0); ok {
		t.Error("the least recently used survived")
	}
	if n, b := c.Len(); n != 3 || b != 90 {
		t.Errorf("%d entries, %d bytes", n, b)
	}
	c.Get("1", t0) // 1 is now the most recent; 2 the least
	c.Put("4", 4, 30, t0, time.Hour, time.Hour)
	if _, ok := c.Get("2", t0); ok {
		t.Error("2 should have been evicted")
	}
	for _, k := range []string{"1", "3", "4"} {
		if _, ok := c.Get(k, t0); !ok {
			t.Errorf("%s evicted", k)
		}
	}

	// Replacing counts the new size only.
	c.Put("4", 40, 10, t0, time.Hour, time.Hour)
	if n, b := c.Len(); n != 3 || b != 70 {
		t.Errorf("after replace: %d entries, %d bytes", n, b)
	}
	// Larger than the whole LRU: not stored, and the old value dropped.
	c.Put("4", 400, 101, t0, time.Hour, time.Hour)
	if _, ok := c.Get("4", t0); ok {
		t.Error("an oversized value was stored")
	}
	if n, b := c.Len(); n != 2 || b != 60 {
		t.Errorf("after oversize: %d entries, %d bytes", n, b)
	}
	// One large value evicts several.
	c.Put("big", -1, 100, t0, time.Hour, time.Hour)
	if n, b := c.Len(); n != 1 || b != 100 {
		t.Errorf("after big: %d entries, %d bytes", n, b)
	}
}

func TestGroupSharesOneCall(t *testing.T) {
	var g Group[int]
	var calls atomic.Int32
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 42, nil
	}
	var wg sync.WaitGroup
	results := make([]int, 5)
	for i := range results {
		wg.Go(func() {
			v, err := g.Do(context.Background(), "k", fn)
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		})
	}
	waitFor(t, func() bool { return g.Waiting("k") == 5 })
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d calls, want 1", calls.Load())
	}
	for i, v := range results {
		if v != 42 {
			t.Errorf("caller %d got %d", i, v)
		}
	}
	// Done: the next caller starts a new call.
	g.Do(context.Background(), "k", func(context.Context) (int, error) { calls.Add(1); return 0, nil })
	if calls.Load() != 2 {
		t.Errorf("%d calls after the first ended, want 2", calls.Load())
	}
}

// A caller that leaves does not cancel the call for the others; the last
// one to leave does.
func TestGroupCancelsWhenAllLeave(t *testing.T) {
	var g Group[int]
	cancelled := make(chan struct{})
	release := make(chan struct{})
	fn := func(ctx context.Context) (int, error) {
		select {
		case <-ctx.Done():
			close(cancelled)
			return 0, ctx.Err()
		case <-release:
			return 7, nil
		}
	}
	ctx1, leave1 := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := g.Do(ctx1, "k", fn); first <- err }()
	waitFor(t, func() bool { return g.Waiting("k") == 1 })
	second := make(chan int, 1)
	go func() { v, _ := g.Do(context.Background(), "k", fn); second <- v }()
	waitFor(t, func() bool { return g.Waiting("k") == 2 })

	leave1()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Errorf("the leaving caller got %v", err)
	}
	select {
	case <-cancelled:
		t.Fatal("the call was cancelled while a caller still waited")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if v := <-second; v != 7 {
		t.Errorf("the staying caller got %d", v)
	}

	// Alone and leaving: the call is cancelled.
	cancelled = make(chan struct{})
	release = make(chan struct{})
	ctx, leave := context.WithCancel(context.Background())
	go func() {
		for g.Waiting("k") != 1 {
			time.Sleep(time.Millisecond)
		}
		leave()
	}()
	if _, err := g.Do(ctx, "k", fn); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the call was not cancelled when its last caller left")
	}
}

func TestGroupPanic(t *testing.T) {
	var g Group[int]
	_, err := g.Do(context.Background(), "k", func(context.Context) (int, error) { panic("bug") })
	if p, ok := errors.AsType[*PanicError](err); !ok || p.Value != "bug" || len(p.Stack) == 0 {
		t.Errorf("err = %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}
