// Package local is holocron's own fetcher as a provider of /v1/fetch
// (SPEC.md §5.3): the guarded client of internal/fetch and the extraction
// of internal/extract, its concurrency bounded by extraction slots so the
// memory an extraction takes stays bounded (§8).
package local

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/provider"
)

// Fetcher is what Provider needs of fetch.Client.
type Fetcher interface {
	Fetch(ctx context.Context, raw string, done func()) (*fetch.Page, error)
}

// BusyError is every extraction slot staying taken (busy).
type BusyError struct{ Msg string }

func (e *BusyError) Error() string { return e.Msg }

// Provider fetches pages itself. It is safe for concurrent use.
type Provider struct {
	fetcher  Fetcher
	slots    chan struct{}
	slotWait time.Duration
}

// New returns a Provider of f with n extraction slots, each waited for at
// most wait.
func New(f Fetcher, n int, wait time.Duration) *Provider {
	return &Provider{fetcher: f, slots: make(chan struct{}, n), slotWait: wait}
}

func (p *Provider) Name() string { return "local" }

// Health is always ok: the fetcher needs nothing but the network.
func (p *Provider) Health(context.Context) error { return nil }

// Slots reports the extraction slots taken and their number.
func (p *Provider) Slots() (inUse, max int) { return len(p.slots), cap(p.slots) }

// Extract fetches and extracts r.URL. It has no notion of a focus: the
// page is the whole text, and Focused is false.
func (p *Provider) Extract(ctx context.Context, r provider.ExtractRequest) (*provider.Page, error) {
	release, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	// The slot is released by Fetch when all its work ends, which may be
	// after a timed-out request has been answered (§7.1).
	pg, err := p.fetcher.Fetch(ctx, r.URL, release)
	if err != nil {
		return nil, err
	}
	return &provider.Page{
		URL:         r.URL,
		FinalURL:    pg.FinalURL,
		Status:      pg.Status,
		ContentType: pg.ContentType,
		Title:       pg.Title,
		Text:        pg.Text,
		Truncated:   pg.Truncated,
	}, nil
}

// acquire takes an extraction slot, waiting at most slotWait; release
// gives it back exactly once.
func (p *Provider) acquire(ctx context.Context) (release func(), err error) {
	var once sync.Once
	release = func() { once.Do(func() { <-p.slots }) }
	select {
	case p.slots <- struct{}{}:
		return release, nil
	default:
	}
	t := time.NewTimer(p.slotWait)
	defer t.Stop()
	select {
	case p.slots <- struct{}{}:
		return release, nil
	case <-t.C:
		return nil, &BusyError{fmt.Sprintf("all %d extraction slots stayed taken for %s", cap(p.slots), p.slotWait)}
	case <-ctx.Done():
		return nil, &BusyError{"the request ended while waiting for an extraction slot"}
	}
}
