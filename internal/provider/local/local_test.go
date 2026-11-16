package local

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tildaslashalef/holocron/internal/fetch"
	"github.com/tildaslashalef/holocron/internal/provider"
)

type fakeFetcher struct {
	block chan struct{}
}

func (f *fakeFetcher) Fetch(ctx context.Context, raw string, done func()) (*fetch.Page, error) {
	if block := f.block; block != nil {
		go func() { <-block; done() }()
		return &fetch.Page{URL: raw, FinalURL: raw + "/", Status: 200, ContentType: "text/html", Title: "T", Text: "x"}, nil
	}
	defer done()
	return &fetch.Page{URL: raw, FinalURL: raw, Status: 200, ContentType: "text/plain", Text: "plain"}, nil
}

func TestExtract(t *testing.T) {
	p := New(&fakeFetcher{}, 2, 10*time.Millisecond)
	pg, err := p.Extract(context.Background(), provider.ExtractRequest{URL: "https://example.org/", Focus: "ignored"})
	if err != nil || pg.Text != "plain" || pg.Focused || pg.Status != 200 || pg.ContentType != "text/plain" {
		t.Fatalf("%+v %v", pg, err)
	}
	if n, m := p.Slots(); n != 0 || m != 2 || p.Name() != "local" || p.Health(context.Background()) != nil {
		t.Errorf("slots %d/%d", n, m)
	}
}

// An extraction holds its slot until its work ends, after its answer.
func TestSlotsBusy(t *testing.T) {
	f := &fakeFetcher{block: make(chan struct{})}
	p := New(f, 1, 20*time.Millisecond)
	if _, err := p.Extract(context.Background(), provider.ExtractRequest{URL: "https://a.example/"}); err != nil {
		t.Fatal(err)
	}
	_, err := p.Extract(context.Background(), provider.ExtractRequest{URL: "https://b.example/"})
	if _, ok := errors.AsType[*BusyError](err); !ok {
		t.Errorf("second while the first works: %v", err)
	}
	close(f.block)
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		if n, _ := p.Slots(); n == 0 || time.Now().After(deadline) {
			break
		}
	}
	f.block = nil
	if _, err := p.Extract(context.Background(), provider.ExtractRequest{URL: "https://c.example/"}); err != nil {
		t.Errorf("after: %v", err)
	}
}
