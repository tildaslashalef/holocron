package api

import (
	"context"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	rtmetrics "runtime/metrics"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/tildaslashalef/holocron/internal/provider"
)

// The dashboard's numbers (§8.3): in memory, since the process started,
// with the last 24 hours in one-minute buckets.
const (
	statsMinutes = 24 * 60
	recentMax    = 50
	latencyKeep  = 512 // the last durations kept per endpoint for percentiles
)

// statEndpoints are the endpoints the dashboard counts, in its order.
var statEndpoints = []string{"/v1/search", "/v1/context", "/v1/fetch"}

func endpointIndex(path string) int {
	return slices.Index(statEndpoints, path)
}

// minute is one bucket of the last 24 hours.
type minute struct {
	stamp  int64 // Unix minute; a bucket of another minute is stale
	req    [3]int64
	errs   [3]int64
	hits   [3]int64
	cost   int64 // micro-dollars billed
	saved  int64 // micro-dollars the cache saved
	ms     [3]int64
	billed map[string]int64 // micro-dollars by provider
}

// providerStats count one provider since start.
type providerStats struct {
	answered  int64
	fallbacks map[string]int64 // by reason
	billed    provider.Bill
}

// recent is one request of the recent list.
type recent struct {
	At       time.Time `json:"at"`
	Endpoint string    `json:"endpoint"`
	Query    string    `json:"query,omitempty"`
	URL      string    `json:"url,omitempty"`
	Host     string    `json:"host,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Fallback []string  `json:"fallback,omitempty"`
	Cache    string    `json:"cache,omitempty"`
	Status   int       `json:"status"`
	Code     string    `json:"code,omitempty"`
	Results  int       `json:"results"`
	Ms       int64     `json:"ms"`
	CostUSD  float64   `json:"cost_usd"`
}

// stats is the dashboard's state. Safe for concurrent use.
type stats struct {
	mu        sync.Mutex
	started   time.Time
	now       func() time.Time
	minutes   [statsMinutes]minute
	totalReq  [3]int64
	totalErr  [3]int64
	totalHits [3]int64
	cost      int64
	saved     int64
	codes     map[string]int64
	cacheBy   map[string]map[string]int64 // kind → hit/miss/stale
	providers map[string]*providerStats
	latency   [3][]int64
	latencyAt [3]int
	recent    []recent
	busy      int64
}

func newStats(now time.Time) *stats {
	return &stats{
		started:   now,
		now:       time.Now,
		codes:     map[string]int64{},
		cacheBy:   map[string]map[string]int64{},
		providers: map[string]*providerStats{},
	}
}

func (s *stats) provider(name string) *providerStats {
	p := s.providers[name]
	if p == nil {
		p = &providerStats{fallbacks: map[string]int64{}}
		s.providers[name] = p
	}
	return p
}

// bucket is the current minute's bucket, reset when it held an older one.
func (s *stats) bucket(t time.Time) *minute {
	stamp := t.Unix() / 60
	b := &s.minutes[stamp%statsMinutes]
	if b.stamp != stamp {
		*b = minute{stamp: stamp}
	}
	return b
}

// record counts one request of the three endpoints.
func (s *stats) record(r *http.Request, e *entry, d time.Duration, logQueries bool) {
	i := endpointIndex(r.URL.Path)
	if i < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b := s.bucket(now)
	ms := d.Milliseconds()
	b.req[i]++
	b.ms[i] += ms
	s.totalReq[i]++
	if e.code != "" {
		b.errs[i]++
		s.totalErr[i]++
		s.codes[e.code]++
		if e.code == "busy" {
			s.busy++
		}
	}
	if e.cache == cacheHit || e.cache == cacheStale {
		b.hits[i]++
		s.totalHits[i]++
	}
	if e.cache != "" {
		kind := cacheKinds[r.URL.Path]
		if s.cacheBy[kind] == nil {
			s.cacheBy[kind] = map[string]int64{}
		}
		s.cacheBy[kind][e.cache]++
	}
	var cost int64
	for name, bill := range e.billed {
		cost += bill.Micros
		p := s.provider(name)
		p.billed = p.billed.Add(bill)
		if b.billed == nil {
			b.billed = map[string]int64{}
		}
		b.billed[name] += bill.Micros
	}
	b.cost += cost
	s.cost += cost
	b.saved += e.saved
	s.saved += e.saved
	if e.answeredBy != "" {
		s.provider(e.answeredBy).answered++
	}
	var fallbacks []string
	for _, f := range e.trace.Fallbacks() {
		s.provider(f.Provider).fallbacks[f.Reason]++
		fallbacks = append(fallbacks, f.Provider+": "+f.Reason)
	}
	if len(s.latency[i]) < latencyKeep {
		s.latency[i] = append(s.latency[i], ms)
	} else {
		s.latency[i][s.latencyAt[i]] = ms
		s.latencyAt[i] = (s.latencyAt[i] + 1) % latencyKeep
	}
	rc := recent{
		At: now.UTC().Truncate(time.Second), Endpoint: r.URL.Path, Host: e.host, Provider: e.answeredBy, Fallback: fallbacks,
		Cache: e.cache, Status: e.status, Code: e.code, Results: e.results, Ms: ms, CostUSD: float64(cost) / 1e6,
	}
	if logQueries {
		rc.Query, rc.URL = e.query, e.url
	}
	if rc.Host == "" && e.url != "" {
		if u, err := url.Parse(e.url); err == nil {
			rc.Host = u.Hostname()
		}
	}
	s.recent = append(s.recent, rc)
	if len(s.recent) > recentMax {
		s.recent = s.recent[len(s.recent)-recentMax:]
	}
}

// StatsBody is /v1/stats's answer: the dashboard's data (§8.3). It is
// for the operator, not part of the client contract.
type StatsBody struct {
	Version    string              `json:"version"`
	GoVersion  string              `json:"go_version"`
	Started    time.Time           `json:"started"`
	Now        time.Time           `json:"now"`
	UptimeS    int64               `json:"uptime_s"`
	LogQueries bool                `json:"log_queries"`
	Routes     map[string][]string `json:"routes"`
	Providers  []ProviderStats     `json:"providers"`
	Totals     Totals              `json:"totals"`
	Last24h    Totals              `json:"last_24h"`
	Endpoints  []EndpointStats     `json:"endpoints"`
	Series     Series              `json:"series"`
	Codes      map[string]int64    `json:"codes"`
	Cache      []CacheKindStats    `json:"cache"`
	Slots      []SlotStats         `json:"slots"`
	Busy       int64               `json:"busy"`
	Memory     MemoryStats         `json:"memory"`
	Recent     []recent            `json:"recent"`
}

// ProviderStats is one provider on the dashboard.
type ProviderStats struct {
	Name      string           `json:"name"`
	State     string           `json:"state"`
	Why       string           `json:"why,omitempty"`
	Endpoints []string         `json:"endpoints"`
	Metered   bool             `json:"metered"`
	Answered  int64            `json:"answered"`
	Fallbacks map[string]int64 `json:"fallbacks"`
	Units     int64            `json:"billed_units"`
	CostUSD   float64          `json:"cost_usd"`
	Cost24h   float64          `json:"cost_24h_usd"`
	Quota     *QuotaStats      `json:"quota,omitempty"`
}

// Totals are counts over a span.
type Totals struct {
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	Hits     int64   `json:"cache_hits"`
	CostUSD  float64 `json:"cost_usd"`
	SavedUSD float64 `json:"saved_usd"`
}

// EndpointStats is one endpoint's counts and latency.
type EndpointStats struct {
	Endpoint string `json:"endpoint"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
	Hits     int64  `json:"cache_hits"`
	P50Ms    int64  `json:"p50_ms"`
	P95Ms    int64  `json:"p95_ms"`
	Samples  int    `json:"latency_samples"`
}

// Series is the last 24 hours in 5-minute steps, oldest first.
type Series struct {
	StepS    int                `json:"step_s"`
	Start    time.Time          `json:"start"`
	Requests map[string][]int64 `json:"requests"`
	Errors   []int64            `json:"errors"`
	CostUSD  []float64          `json:"cost_usd"`
}

// CacheKindStats is one cache's size and lookups.
type CacheKindStats struct {
	Kind    string `json:"kind"`
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
	Hit     int64  `json:"hit"`
	Miss    int64  `json:"miss"`
	Stale   int64  `json:"stale"`
}

// SlotStats is one pool of slots.
type SlotStats struct {
	Pool  string `json:"pool"`
	InUse int    `json:"in_use"`
	Max   int    `json:"max"`
}

// MemoryStats is the Go runtime's view.
type MemoryStats struct {
	HeapBytes  uint64 `json:"heap_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
	LimitBytes int64  `json:"limit_bytes"` // GOMEMLIMIT; math.MaxInt64 when unset
	Goroutines uint64 `json:"goroutines"`
}

const seriesStep = 5 // minutes

// snapshot builds the body; health is asked outside the lock.
func (s *Server) snapshot(ctx context.Context) StatsBody {
	st := s.stats
	type state struct{ state, why string }
	states := map[string]state{}
	for _, p := range s.routes.Providers() {
		if err := p.Health(ctx); err != nil {
			states[p.Name()] = state{"down", err.Error()}
		} else {
			states[p.Name()] = state{"ok", ""}
		}
	}
	routes := s.routeNames()

	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	out := StatsBody{
		Version: s.version, GoVersion: runtime.Version(), Started: st.started.UTC(), Now: now.UTC(),
		UptimeS: int64(now.Sub(st.started) / time.Second), LogQueries: s.logQueries, Routes: routes,
		Codes: map[string]int64{}, Busy: st.busy, Recent: []recent{},
	}
	for k, v := range st.codes {
		out.Codes[k] = v
	}

	// The last 24 hours, oldest first, in seriesStep steps.
	cur := now.Unix() / 60
	first := cur - statsMinutes + 1
	first -= first % seriesStep
	steps := int((cur-first)/seriesStep) + 1
	out.Series = Series{StepS: seriesStep * 60, Start: time.Unix(first*60, 0).UTC(), Requests: map[string][]int64{},
		Errors: make([]int64, steps), CostUSD: make([]float64, steps)}
	for _, ep := range statEndpoints {
		out.Series.Requests[ep] = make([]int64, steps)
	}
	billed24 := map[string]int64{}
	var last Totals
	var cost24, saved24 int64
	for m := first; m <= cur; m++ {
		b := &st.minutes[m%statsMinutes]
		if b.stamp != m || m <= cur-statsMinutes {
			continue
		}
		step := int((m - first) / seriesStep)
		for i, ep := range statEndpoints {
			out.Series.Requests[ep][step] += b.req[i]
			out.Series.Errors[step] += b.errs[i]
			last.Requests += b.req[i]
			last.Errors += b.errs[i]
			last.Hits += b.hits[i]
		}
		out.Series.CostUSD[step] += float64(b.cost) / 1e6
		cost24 += b.cost
		saved24 += b.saved
		for name, v := range b.billed {
			billed24[name] += v
		}
	}
	last.CostUSD, last.SavedUSD = float64(cost24)/1e6, float64(saved24)/1e6
	out.Last24h = last

	var total Totals
	for i, ep := range statEndpoints {
		total.Requests += st.totalReq[i]
		total.Errors += st.totalErr[i]
		total.Hits += st.totalHits[i]
		p50, p95 := percentiles(st.latency[i])
		out.Endpoints = append(out.Endpoints, EndpointStats{Endpoint: ep, Requests: st.totalReq[i], Errors: st.totalErr[i],
			Hits: st.totalHits[i], P50Ms: p50, P95Ms: p95, Samples: len(st.latency[i])})
	}
	total.CostUSD, total.SavedUSD = float64(st.cost)/1e6, float64(st.saved)/1e6
	out.Totals = total

	for _, p := range s.routes.Providers() {
		name := p.Name()
		ps := ProviderStats{Name: name, State: states[name].state, Why: states[name].why, Fallbacks: map[string]int64{}, Endpoints: []string{}}
		for _, ep := range []string{"/v1/search", "/v1/context", "/v1/fetch"} {
			if slices.Contains(routes[ep], name) {
				ps.Endpoints = append(ps.Endpoints, ep)
			}
		}
		if c := st.providers[name]; c != nil {
			ps.Answered = c.answered
			for k, v := range c.fallbacks {
				ps.Fallbacks[k] = v
			}
		}
		if b, ok := p.(provider.Biller); ok {
			ps.Metered = true
			bill := b.Billed()
			ps.Units, ps.CostUSD = bill.Units, float64(bill.Micros)/1e6
			ps.Cost24h = float64(billed24[name]) / 1e6
		}
		if q, ok := p.(provider.Quotaer); ok {
			if quota, ok := q.Quota(); ok {
				ps.Quota = &QuotaStats{Remaining: quota.Remaining, ResetsInS: int64(quota.ResetsIn / time.Second)}
			}
		}
		out.Providers = append(out.Providers, ps)
	}

	for _, k := range s.cache.kinds() {
		n, bytes := k.len()
		c := st.cacheBy[k.kind]
		out.Cache = append(out.Cache, CacheKindStats{Kind: k.kind, Entries: n, Bytes: bytes, Hit: c[cacheHit], Miss: c[cacheMiss], Stale: c[cacheStale]})
	}
	for _, p := range s.pools() {
		out.Slots = append(out.Slots, SlotStats{Pool: p.name, InUse: p.inUse, Max: p.max})
	}
	samples := []rtmetrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/sched/goroutines:goroutines"},
	}
	rtmetrics.Read(samples)
	u := func(i int) uint64 {
		if samples[i].Value.Kind() == rtmetrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	out.Memory = MemoryStats{HeapBytes: u(0), TotalBytes: u(1), Goroutines: u(2), LimitBytes: debug.SetMemoryLimit(-1)}
	for i := len(st.recent) - 1; i >= 0; i-- {
		out.Recent = append(out.Recent, st.recent[i])
	}
	return out
}

// percentiles are the 50th and 95th of ms, nearest rank.
func percentiles(ms []int64) (p50, p95 int64) {
	if len(ms) == 0 {
		return 0, 0
	}
	s := append([]int64(nil), ms...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) int64 {
		i := int(p*float64(len(s))+0.999999) - 1
		return s[min(max(i, 0), len(s)-1)]
	}
	return at(0.50), at(0.95)
}

func (s *Server) serveStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), HealthTimeout)
	defer cancel()
	write(w, http.StatusOK, s.snapshot(ctx))
}
