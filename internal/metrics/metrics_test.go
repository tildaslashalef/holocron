package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	c := NewCounter("t_requests_total", "Requests.\nTwo lines, a \\ too.", "endpoint", "code")
	c.Inc("/v1/search", "")
	c.Add(2, "/v1/fetch", `odd "value" \ with`+"\nnewline")
	bare := NewCounter("t_bare_total", "Never counted.")
	h := NewHistogram("t_seconds", "Durations.", []float64{0.1, 1}, "endpoint")
	h.Observe(0.05, "/a")
	h.Observe(0.5, "/a")
	h.Observe(3, "/a")

	var b strings.Builder
	c.Write(&b)
	bare.Write(&b)
	h.Write(&b)
	Gauge(&b, "t_up", "Up.", LabeledValue{[][2]string{{"provider", "brave"}}, 1})
	CounterFunc(&b, "t_gc_total", "GC.", 7)

	p, err := Parse(b.String())
	if err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	for key, want := range map[string]float64{
		`t_requests_total{endpoint="/v1/search",code=""}`:                              1,
		`t_requests_total{endpoint="/v1/fetch",code="odd \"value\" \\ with\nnewline"}`: 2,
		`t_bare_total`: 0,
		`t_seconds_bucket{endpoint="/a",le="0.1"}`:  1,
		`t_seconds_bucket{endpoint="/a",le="1"}`:    2,
		`t_seconds_bucket{endpoint="/a",le="+Inf"}`: 3,
		`t_seconds_sum{endpoint="/a"}`:              3.55,
		`t_seconds_count{endpoint="/a"}`:            3,
		`t_up{provider="brave"}`:                    1,
		`t_gc_total`:                                7,
	} {
		if got, ok := p.Samples[key]; !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v\n%s", key, got, ok, want, b.String())
		}
	}
	if p.Types["t_seconds"] != "histogram" || p.Types["t_up"] != "gauge" || p.Types["t_requests_total"] != "counter" {
		t.Errorf("types %v", p.Types)
	}
	if !strings.Contains(b.String(), "# HELP t_requests_total Requests.\\nTwo lines, a \\\\ too.\n") {
		t.Errorf("help not escaped:\n%s", b.String())
	}
	if c.Value("/v1/fetch", `odd "value" \ with`+"\nnewline") != 2 || c.Value("none", "") != 0 {
		t.Error("Value")
	}
}

func TestParseRejects(t *testing.T) {
	for _, text := range []string{
		"x 1\n", // no HELP or TYPE
		"# HELP x a\n# TYPE x counter\n# HELP x b\n# TYPE x counter\nx 1\n",
		"# HELP x a\n# TYPE x counter\ny 1\n",
		"# HELP x a\n# TYPE x counter\nx{a=\"b\\q\"} 1\n",
		"# HELP x a\n# TYPE x counter\nx{a=\"b} 1\n",
		"# HELP x a\n# TYPE x counter\nx{a=\"b\",a=\"c\"} 1\n",
		"# HELP x a\n# TYPE x counter\nx one\n",
		"# HELP x a\n# TYPE x summary\nx 1\n",
		"# HELP x a\n# TYPE x histogram\nx 1\n",
		"# HELP x a\n# TYPE x counter\nx 1\nx 1\n",
	} {
		if _, err := Parse(text); err == nil {
			t.Errorf("accepted:\n%s", text)
		}
	}
}

func TestLabelCountChecked(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a wrong number of label values did not panic")
		}
	}()
	NewCounter("x", "x", "a").Inc()
}
