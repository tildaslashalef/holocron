package provider

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A provider's key never follows a redirect to another host.
func TestAPIClientNoRedirect(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer api.Close()
	req, _ := http.NewRequest(http.MethodGet, api.URL, nil)
	req.Header.Set("X-Api-Key", "secret")
	resp, err := APIClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || reached.Load() != 0 {
		t.Errorf("status %d, other host reached %d times", resp.StatusCode, reached.Load())
	}
}

func TestHoldAndMeter(t *testing.T) {
	var h Hold
	now := time.Now()
	if h.Err(now) != nil {
		t.Error("held at start")
	}
	h.Set(now, time.Minute, ReasonQuota, "used up")
	if u, ok := h.Err(now.Add(time.Second)).(*UnavailableError); !ok || u.Reason != ReasonQuota {
		t.Error("not held")
	}
	if h.Err(now.Add(2*time.Minute)) != nil {
		t.Error("held past its end")
	}
	var m Meter
	m.Add(Bill{1, 10})
	m.Add(Bill{2, 20})
	if m.Billed() != (Bill{3, 30}) {
		t.Error("meter")
	}
}
