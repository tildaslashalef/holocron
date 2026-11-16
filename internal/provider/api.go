package provider

import (
	"crypto/tls"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// APIClient is the HTTP client of a provider's own API (§5.5): no proxy
// from the environment, and no redirects followed, so a key sent in a
// header never reaches another host. A redirect is answered to the
// provider as its 3xx status, which it reports as a failure.
func APIClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			ForceAttemptHTTP2:   true,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Hold stops a provider's calls for a while after a failure more calls
// cannot fix: a refused key, a used-up quota (§5.5). Safe for concurrent
// use.
type Hold struct {
	mu     sync.Mutex
	until  time.Time
	why    string
	reason string
}

// Set holds calls until now+d and returns the failure that explains it.
func (h *Hold) Set(now time.Time, d time.Duration, reason, why string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.until, h.why, h.reason = now.Add(d), why, reason
	return &UnavailableError{Msg: why, Reason: reason}
}

// Err is the hold's failure while it lasts, else nil.
func (h *Hold) Err(now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if now.Before(h.until) {
		return &UnavailableError{Msg: h.why, Reason: h.reason}
	}
	return nil
}

// Meter counts a provider's bill since the process started. Safe for
// concurrent use.
type Meter struct {
	units, micros atomic.Int64
}

// Add counts b.
func (m *Meter) Add(b Bill) {
	m.units.Add(b.Units)
	m.micros.Add(b.Micros)
}

// Billed is the total so far.
func (m *Meter) Billed() Bill {
	return Bill{m.units.Load(), m.micros.Load()}
}
