package provider

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Chain is one endpoint's providers in order (§5.4): the next is asked
// only when one is unavailable, within the request's budget. A member
// that is a Budgeter gets at most its own budget while another follows
// it. The zero Chain, and a nil one, has no members.
type Chain struct {
	list []Provider
}

// NewChain returns the chain of ps.
func NewChain(ps ...Provider) *Chain {
	return &Chain{list: ps}
}

// Name is the members' names joined by commas: "parallel,brave"; "" for
// none.
func (c *Chain) Name() string {
	if c == nil {
		return ""
	}
	names := make([]string, len(c.list))
	for i, p := range c.list {
		names[i] = p.Name()
	}
	return strings.Join(names, ",")
}

// Names is the members' names in order.
func (c *Chain) Names() []string {
	if c == nil || len(c.list) == 0 {
		return []string{}
	}
	return strings.Split(c.Name(), ",")
}

// Members returns the providers in order.
func (c *Chain) Members() []Provider {
	if c == nil {
		return nil
	}
	return c.list
}

// Empty reports a chain with no members.
func (c *Chain) Empty() bool { return c == nil || len(c.list) == 0 }

// Answered is how a chained request was answered: the member that
// answered, and the members that failed before it, in order.
type Answered struct {
	Provider string
	Failed   []string
}

// Search asks each member that is a Searcher in turn.
func (c *Chain) Search(ctx context.Context, r SearchRequest) (Results, Answered, error) {
	return ask(ctx, c, func(ctx context.Context, p Provider) (Results, error) {
		return p.(Searcher).Search(ctx, r)
	}, func(p Provider) bool { _, ok := p.(Searcher); return ok })
}

// Context asks each member that is a Contexter in turn.
func (c *Chain) Context(ctx context.Context, r ContextRequest) (Context, Answered, error) {
	return ask(ctx, c, func(ctx context.Context, p Provider) (Context, error) {
		return p.(Contexter).Context(ctx, r)
	}, func(p Provider) bool { _, ok := p.(Contexter); return ok })
}

// Extract asks each member that is an Extractor in turn.
func (c *Chain) Extract(ctx context.Context, r ExtractRequest) (*Page, Answered, error) {
	return ask(ctx, c, func(ctx context.Context, p Provider) (*Page, error) {
		return p.(Extractor).Extract(ctx, r)
	}, func(p Provider) bool { _, ok := p.(Extractor); return ok })
}

// ask is one chained request. Each member moved past is recorded in the
// context's trace with its reason.
func ask[A any](ctx context.Context, c *Chain, call func(context.Context, Provider) (A, error), able func(Provider) bool) (A, Answered, error) {
	var zero A
	var list []Provider
	for _, p := range c.Members() {
		if able(p) {
			list = append(list, p)
		}
	}
	if len(list) == 0 {
		return zero, Answered{}, &NotAvailableError{"no configured provider answers this endpoint"}
	}
	trace := TraceOf(ctx)
	var failed, msgs []string
	for i, p := range list {
		last := i == len(list)-1
		a, err, own := askOne(ctx, p, last, call)
		if err == nil {
			return a, Answered{Provider: p.Name(), Failed: failed}, nil
		}
		var reason string
		if u, ok := errors.AsType[*UnavailableError](err); ok {
			reason = u.Reason
		} else if _, ok := errors.AsType[*TimeoutError](err); ok && own {
			reason = ReasonTimeout
		} else {
			// A final answer (a page that does not exist, a query refused),
			// or the request's own budget gone: no member can do better.
			return zero, Answered{Failed: failed}, err
		}
		if reason == "" {
			reason = ReasonError
		}
		if !last {
			trace.Fallback(p.Name(), reason)
		}
		failed, msgs = append(failed, p.Name()), append(msgs, err.Error())
		if last && reason == ReasonTimeout {
			return zero, Answered{Failed: failed}, &TimeoutError{strings.Join(msgs, "; ")}
		}
	}
	return zero, Answered{Failed: failed}, &UnavailableError{Msg: strings.Join(msgs, "; ")}
}

// askOne asks one member, under its share of the remaining budget when it
// is a Budgeter and not last; own reports a timeout of that share, not of
// the request's budget.
func askOne[A any](ctx context.Context, p Provider, last bool, call func(context.Context, Provider) (A, error)) (A, error, bool) {
	b, ok := p.(Budgeter)
	deadline, has := ctx.Deadline()
	if !ok || last || !has || b.BudgetShare() <= 0 || b.BudgetShare() >= 1 {
		a, err := call(ctx, p)
		return a, err, false
	}
	share := time.Duration(float64(time.Until(deadline)) * b.BudgetShare())
	sub, cancel := context.WithTimeout(ctx, share)
	defer cancel()
	a, err := call(sub, p)
	own := err != nil && sub.Err() != nil && ctx.Err() == nil
	if own {
		if _, isTimeout := errors.AsType[*TimeoutError](err); !isTimeout {
			err = &TimeoutError{p.Name() + " did not answer within its share of the budget"}
		}
	}
	return a, err, own
}

// can reports whether a member of c satisfies f.
func (c *Chain) can(f func(Provider) bool) bool {
	for _, p := range c.Members() {
		if f(p) {
			return true
		}
	}
	return false
}

// CanSearch reports a Searcher among the members.
func (c *Chain) CanSearch() bool {
	return c.can(func(p Provider) bool { _, ok := p.(Searcher); return ok })
}

// CanContext reports a Contexter among the members.
func (c *Chain) CanContext() bool {
	return c.can(func(p Provider) bool { _, ok := p.(Contexter); return ok })
}

// CanExtract reports an Extractor among the members.
func (c *Chain) CanExtract() bool {
	return c.can(func(p Provider) bool { _, ok := p.(Extractor); return ok })
}

// Routes are the chains of the three endpoints (§5.4).
type Routes struct {
	Search, Context, Fetch *Chain
}

// Providers returns every distinct provider of the routes, in the order
// they first appear.
func (r Routes) Providers() []Provider {
	var out []Provider
	seen := map[string]bool{}
	for _, c := range []*Chain{r.Search, r.Context, r.Fetch} {
		for _, p := range c.Members() {
			if !seen[p.Name()] {
				seen[p.Name()] = true
				out = append(out, p)
			}
		}
	}
	return out
}
