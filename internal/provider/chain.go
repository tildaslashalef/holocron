package provider

import (
	"context"
	"errors"
	"strings"
)

// Chain asks its providers in order, the next only when one answers
// no_engines (§5.3). It is a Provider and a Contexter.
type Chain struct {
	list []Provider
}

// NewChain returns the chain of ps: ps[0] itself when it is alone.
func NewChain(ps ...Provider) Provider {
	if len(ps) == 1 {
		return ps[0]
	}
	return &Chain{list: ps}
}

// Name is the members' names joined by commas: "brave,searxng".
func (c *Chain) Name() string {
	names := make([]string, len(c.list))
	for i, p := range c.list {
		names[i] = p.Name()
	}
	return strings.Join(names, ",")
}

// Health is the first member's failure, or nil.
func (c *Chain) Health(ctx context.Context) error {
	for _, p := range c.list {
		if err := p.Health(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Search asks each member until one answers with results, or without
// failed engines; the members that failed before it are listed first in
// its EnginesFailed.
func (c *Chain) Search(ctx context.Context, r SearchRequest) (Results, error) {
	return ask(TraceOf(ctx), c.list, func(p Provider) (Results, error) {
		return p.Search(ctx, r)
	}, func(r *Results) (bool, *string, *[]string) {
		return len(r.Items) > 0 || len(r.EnginesFailed) == 0, &r.Provider, &r.EnginesFailed
	})
}

// Context is Search for the members with context.
func (c *Chain) Context(ctx context.Context, r ContextRequest) (Context, error) {
	var list []Provider
	for _, p := range c.list {
		if _, ok := p.(Contexter); ok {
			list = append(list, p)
		}
	}
	if len(list) == 0 {
		return Context{}, notAvailable(c)
	}
	return ask(TraceOf(ctx), list, func(p Provider) (Context, error) {
		return p.(Contexter).Context(ctx, r)
	}, func(a *Context) (bool, *string, *[]string) {
		return len(a.Sources) > 0 || len(a.EnginesFailed) == 0, &a.Provider, &a.EnginesFailed
	})
}

// ask is one chained request. call asks a member; fields reports whether
// an answer is final and where its provider and failed engines are. Each
// member moved past is recorded in trace.
func ask[A any](trace *Trace, list []Provider, call func(Provider) (A, error), fields func(*A) (bool, *string, *[]string)) (A, error) {
	var failed, msgs []string
	for i, p := range list {
		a, err := call(p)
		last := i == len(list)-1
		if err == nil {
			final, provider, engines := fields(&a)
			if final || last {
				if *provider == "" {
					*provider = p.Name()
				}
				*engines = append(append([]string{}, failed...), *engines...)
				return a, nil
			}
			err = &UnavailableError{Msg: p.Name() + ": no results while engines failed", Reason: ReasonNoResults}
		}
		u, ok := errors.AsType[*UnavailableError](err)
		if !ok {
			var zero A
			return zero, err
		}
		if !last {
			reason := u.Reason
			if reason == "" {
				reason = ReasonError
			}
			trace.Fallback(p.Name(), reason)
		}
		failed, msgs = append(failed, p.Name()), append(msgs, err.Error())
	}
	var zero A
	return zero, &UnavailableError{Msg: strings.Join(msgs, "; ")}
}

// Members returns a chain's providers, or p alone.
func Members(p Provider) []Provider {
	if c, ok := p.(*Chain); ok {
		return c.list
	}
	return []Provider{p}
}

// HasContext reports whether p, or a member of its chain, has context.
func HasContext(p Provider) bool {
	for _, m := range Members(p) {
		if _, ok := m.(Contexter); ok {
			return true
		}
	}
	return false
}

// ContextOf asks p for context: not_available when neither it nor a
// member of its chain has any.
func ContextOf(ctx context.Context, p Provider, r ContextRequest) (Context, error) {
	switch c := p.(type) {
	case *Chain:
		return c.Context(ctx, r)
	case Contexter:
		return c.Context(ctx, r)
	}
	return Context{}, notAvailable(p)
}

func notAvailable(p Provider) error {
	return &NotAvailableError{p.Name() + " has no context endpoint; use /v1/search and /v1/fetch"}
}
