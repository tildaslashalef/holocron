package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fake struct {
	name     string
	res      Results
	err      error
	health   error
	searches int
	slow     time.Duration // Search waits this long, or for its context
	share    float64
}

func (f *fake) Name() string                     { return f.name }
func (f *fake) Health(ctx context.Context) error { return f.health }
func (f *fake) Search(ctx context.Context, r SearchRequest) (Results, error) {
	f.searches++
	if f.slow > 0 {
		select {
		case <-time.After(f.slow):
		case <-ctx.Done():
			return Results{}, &TimeoutError{f.name + " slow"}
		}
	}
	return f.res, f.err
}

type budgeted struct{ *fake }

func (b budgeted) BudgetShare() float64 { return b.share }

type fakeContext struct {
	*fake
	ctx *Context
}

func (f fakeContext) Context(ctx context.Context, r ContextRequest) (Context, error) {
	return *f.ctx, f.err
}

func TestChainSearch(t *testing.T) {
	ok := Results{Items: []Result{{URL: "https://a.example/"}}}
	for _, tt := range []struct {
		name          string
		first, second *fake
		provider      string
		failed        []string
		fallbacks     []Fallback
		err           string
		secondAsked   bool
	}{
		{"first answers", &fake{name: "parallel", res: ok}, &fake{name: "brave"}, "parallel", nil, nil, "", false},
		{"first answers empty", &fake{name: "parallel"}, &fake{name: "brave", res: ok}, "parallel", nil, nil, "", false},
		{"first unavailable", &fake{name: "parallel", err: &UnavailableError{Msg: "parallel: credit used up", Reason: ReasonQuota}}, &fake{name: "brave", res: ok},
			"brave", []string{"parallel"}, []Fallback{{"parallel", "quota"}}, "", true},
		{"no reason is error", &fake{name: "parallel", err: &UnavailableError{Msg: "parallel answered 500"}}, &fake{name: "brave", res: ok},
			"brave", []string{"parallel"}, []Fallback{{"parallel", "error"}}, "", true},
		{"a request timeout ends it", &fake{name: "parallel", err: &TimeoutError{"slow"}}, &fake{name: "brave", res: ok}, "", nil, nil, "slow", false},
		{"a refused query ends it", &fake{name: "parallel", err: &BadQueryError{"too long"}}, &fake{name: "brave", res: ok}, "", nil, nil, "too long", false},
		{"both unavailable", &fake{name: "parallel", err: &UnavailableError{Msg: "parallel: down"}}, &fake{name: "brave", err: &UnavailableError{Msg: "brave: down"}},
			"", nil, []Fallback{{"parallel", "error"}}, "parallel: down; brave: down", true},
	} {
		c := NewChain(tt.first, tt.second)
		trace := &Trace{}
		_, by, err := c.Search(WithTrace(context.Background(), trace), SearchRequest{Query: "q", N: 10})
		switch {
		case tt.err != "":
			if err == nil || err.Error() != tt.err {
				t.Errorf("%s: err %v, want %q", tt.name, err, tt.err)
			}
		case err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case by.Provider != tt.provider || !slices.Equal(by.Failed, tt.failed):
			t.Errorf("%s: answered %+v", tt.name, by)
		}
		if !slices.Equal(trace.Fallbacks(), tt.fallbacks) {
			t.Errorf("%s: fallbacks %v, want %v", tt.name, trace.Fallbacks(), tt.fallbacks)
		}
		if (tt.second.searches > 0) != tt.secondAsked {
			t.Errorf("%s: second asked %d times", tt.name, tt.second.searches)
		}
	}
}

// A Budgeter that is not last gets its share of the budget, and the next
// member answers in the rest.
func TestChainBudgetShare(t *testing.T) {
	slow := budgeted{&fake{name: "parallel", slow: time.Second, share: 0.3}}
	next := &fake{name: "brave", res: Results{Items: []Result{{URL: "https://b.example/"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	trace := &Trace{}
	start := time.Now()
	_, by, err := NewChain(slow, next).Search(WithTrace(ctx, trace), SearchRequest{})
	if err != nil || by.Provider != "brave" || !slices.Equal(by.Failed, []string{"parallel"}) {
		t.Fatalf("answered %+v, %v", by, err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("took %s: the share was not applied", d)
	}
	if fb := trace.Fallbacks(); len(fb) != 1 || fb[0].Reason != ReasonTimeout {
		t.Errorf("fallbacks %v", fb)
	}

	// Last, a Budgeter has the whole budget.
	alone := budgeted{&fake{name: "parallel", slow: 50 * time.Millisecond, share: 0.1, res: Results{}}}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, _, err := NewChain(alone).Search(ctx2, SearchRequest{}); err != nil {
		t.Errorf("alone: %v", err)
	}
}

func TestChainCapabilities(t *testing.T) {
	plain := &fake{name: "brave"}
	c := NewChain(plain)
	if c.CanContext() || c.CanExtract() || !c.CanSearch() {
		t.Error("capabilities of a Searcher")
	}
	_, _, err := c.Context(context.Background(), ContextRequest{})
	if _, ok := errors.AsType[*NotAvailableError](err); !ok {
		t.Errorf("context without a Contexter: %v", err)
	}
	with := fakeContext{&fake{name: "parallel"}, &Context{Sources: []Source{{URL: "https://a.example/"}}}}
	c = NewChain(plain, with)
	res, by, err := c.Context(context.Background(), ContextRequest{Query: "q"})
	if err != nil || by.Provider != "parallel" || len(by.Failed) != 0 || len(res.Sources) != 1 {
		t.Errorf("context = %+v %+v, %v (only Contexters are asked)", res, by, err)
	}
	var none *Chain
	if !none.Empty() || none.Name() != "" || len(none.Names()) != 0 || none.CanSearch() {
		t.Error("a nil chain")
	}
	if NewChain(plain, with).Name() != "brave,parallel" {
		t.Error("name")
	}
	r := Routes{Search: NewChain(with, plain), Context: NewChain(with), Fetch: NewChain()}
	if ps := r.Providers(); len(ps) != 2 || ps[0].Name() != "parallel" {
		t.Errorf("providers %v", ps)
	}
	if !NoCache(WithNoCache(context.Background())) || NoCache(context.Background()) {
		t.Error("NoCache")
	}
}

func TestTraceBill(t *testing.T) {
	tr := &Trace{}
	tr.Bill("parallel", Bill{1, 1000})
	tr.Bill("parallel", Bill{2, 2000})
	tr.Bill("brave", Bill{1, 5000})
	if b := tr.Billed(); b["parallel"] != (Bill{3, 3000}) || tr.Cost() != 8000 {
		t.Errorf("billed %v cost %d", b, tr.Cost())
	}
	var nilTrace *Trace
	nilTrace.Bill("x", Bill{1, 1})
	if nilTrace.Cost() != 0 || nilTrace.Claim() {
		t.Error("a nil trace")
	}
}

func TestKeys(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	os.WriteFile(good, []byte("  sk-123\n"), 0o600)
	if k, err := ReadKey("parallel", good); err != nil || k != "sk-123" {
		t.Errorf("good: %q %v", k, err)
	}
	open := filepath.Join(dir, "open")
	os.WriteFile(open, []byte("sk-123\n"), 0o644)
	if _, err := ReadKey("parallel", open); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Errorf("open: %v", err)
	}
	two := filepath.Join(dir, "two")
	os.WriteFile(two, []byte("a\nb\n"), 0o600)
	if _, err := ReadKey("parallel", two); err == nil || strings.Contains(err.Error(), "a") && strings.Contains(err.Error(), "b\n") {
		t.Errorf("two lines: %v", err)
	}
	if _, err := ReadKey("parallel", filepath.Join(dir, "missing")); err == nil {
		t.Error("missing")
	}

	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	os.WriteFile(filepath.Join(dir, "brave-key"), []byte("k"), 0o400)
	if KeyFile("brave", "") != filepath.Join(dir, "brave-key") || KeyFile("parallel", "") != "" || KeyFile("parallel", "/x") != "/x" {
		t.Error("KeyFile")
	}
}
