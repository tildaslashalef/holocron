package provider

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type fake struct {
	name     string
	res      Results
	ctx      *Context // nil: no context
	err      error
	health   error
	searches int
}

func (f *fake) Name() string                     { return f.name }
func (f *fake) Health(ctx context.Context) error { return f.health }
func (f *fake) Search(ctx context.Context, r SearchRequest) (Results, error) {
	f.searches++
	return f.res, f.err
}

type fakeContext struct{ *fake }

func (f fakeContext) Context(ctx context.Context, r ContextRequest) (Context, error) {
	return *f.ctx, f.err
}

func TestChainSearch(t *testing.T) {
	ok := Results{Items: []Result{{URL: "https://a.example/"}}, EnginesFailed: []string{"mojeek"}}
	for _, tt := range []struct {
		name          string
		first, second *fake
		provider      string
		failed        []string
		err           string
		secondAsked   bool
	}{
		{"first answers", &fake{name: "brave", res: Results{Items: ok.Items}}, &fake{name: "searxng"}, "brave", []string{}, "", false},
		{"first unavailable", &fake{name: "brave", err: &UnavailableError{Msg: "brave: quota used up"}}, &fake{name: "searxng", res: ok}, "searxng", []string{"brave", "mojeek"}, "", true},
		{"first empty with failures", &fake{name: "brave", res: Results{EnginesFailed: []string{"x"}}}, &fake{name: "searxng", res: ok}, "searxng", []string{"brave", "mojeek"}, "", true},
		{"first empty, nothing failed", &fake{name: "brave", res: Results{EnginesFailed: []string{}}}, &fake{name: "searxng", res: ok}, "brave", []string{}, "", false},
		{"a timeout ends it", &fake{name: "brave", err: &TimeoutError{"slow"}}, &fake{name: "searxng", res: ok}, "", nil, "slow", false},
		{"a refused query ends it", &fake{name: "brave", err: &BadQueryError{"too long"}}, &fake{name: "searxng", res: ok}, "", nil, "too long", false},
		{"both unavailable", &fake{name: "brave", err: &UnavailableError{Msg: "brave: down"}}, &fake{name: "searxng", err: &UnavailableError{Msg: "searxng: down"}}, "", nil, "brave: down; searxng: down", true},
		{"the last's empty answer stands", &fake{name: "brave", err: &UnavailableError{Msg: "brave: down"}}, &fake{name: "searxng", res: Results{EnginesFailed: []string{"ddg"}}}, "searxng", []string{"brave", "ddg"}, "", true},
	} {
		c := NewChain(tt.first, tt.second)
		res, err := c.Search(context.Background(), SearchRequest{Query: "q", N: 10})
		switch {
		case tt.err != "":
			if err == nil || err.Error() != tt.err {
				t.Errorf("%s: err %v, want %q", tt.name, err, tt.err)
			}
		case err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case res.Provider != tt.provider || !slices.Equal(res.EnginesFailed, tt.failed):
			t.Errorf("%s: provider %q failed %v", tt.name, res.Provider, res.EnginesFailed)
		}
		if (tt.second.searches > 0) != tt.secondAsked {
			t.Errorf("%s: second asked %d times", tt.name, tt.second.searches)
		}
	}
}

func TestChainContext(t *testing.T) {
	searx := &fake{name: "searxng"}
	if HasContext(searx) || HasContext(NewChain(searx, &fake{name: "other"})) {
		t.Error("context without a Contexter")
	}
	_, err := ContextOf(context.Background(), searx, ContextRequest{})
	if na, ok := errors.AsType[*NotAvailableError](err); !ok || na.Msg != "searxng has no context endpoint; use /v1/search and /v1/fetch" {
		t.Errorf("searxng alone: %v", err)
	}
	_, err = ContextOf(context.Background(), NewChain(searx, &fake{name: "other"}), ContextRequest{})
	if _, ok := errors.AsType[*NotAvailableError](err); !ok {
		t.Errorf("a chain without context: %v", err)
	}

	brave := fakeContext{&fake{name: "brave", ctx: &Context{Sources: []Source{{URL: "https://a.example/"}}}}}
	c := NewChain(brave, searx)
	if !HasContext(c) || len(Members(c)) != 2 || c.Name() != "brave,searxng" {
		t.Errorf("chain %q", c.Name())
	}
	res, err := ContextOf(context.Background(), c, ContextRequest{Query: "q"})
	if err != nil || res.Provider != "brave" || len(res.Sources) != 1 || res.EnginesFailed == nil {
		t.Errorf("context = %+v, %v", res, err)
	}
	brave.err = &UnavailableError{Msg: "brave: down"}
	if _, err := ContextOf(context.Background(), c, ContextRequest{}); err == nil || err.Error() != "brave: down" {
		t.Errorf("brave down: %v (searxng has no context to fall back to)", err)
	}
}

func TestChainHealthAndSingle(t *testing.T) {
	a, b := &fake{name: "a"}, &fake{name: "b", health: errors.New("down")}
	if p := NewChain(a); p != Provider(a) || len(Members(p)) != 1 {
		t.Error("a chain of one is not the provider itself")
	}
	if err := NewChain(a, b).Health(context.Background()); err == nil {
		t.Error("health hides a member's failure")
	}
	if !NoCache(WithNoCache(context.Background())) || NoCache(context.Background()) {
		t.Error("NoCache")
	}
}
