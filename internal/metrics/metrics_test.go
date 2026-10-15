package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	var r Registry
	c := r.Counter("loam_requests_total", "Requests.", "project", "sandbox")
	c.Inc("p", "web")
	c.Add(2, "p", "web")
	c.Inc("p", `we"ird`)
	h := r.Histogram("loam_seconds", "Durations.", []float64{0.1, 1}, "project")
	h.Observe(0.05, "p")
	h.Observe(0.5, "p")
	h.Observe(5, "p")
	r.GaugeFunc("loam_sandboxes", "Sandboxes.", func() []Sample {
		return []Sample{{Values: []string{"running"}, Value: 2}, {Values: []string{"paused"}, Value: 1}}
	}, "state")

	var b strings.Builder
	r.Write(&b)
	want := `# HELP loam_requests_total Requests.
# TYPE loam_requests_total counter
loam_requests_total{project="p",sandbox="we\"ird"} 1
loam_requests_total{project="p",sandbox="web"} 3
# HELP loam_seconds Durations.
# TYPE loam_seconds histogram
loam_seconds_bucket{project="p",le="0.1"} 1
loam_seconds_bucket{project="p",le="1"} 2
loam_seconds_bucket{project="p",le="+Inf"} 3
loam_seconds_sum{project="p"} 5.55
loam_seconds_count{project="p"} 3
# HELP loam_sandboxes Sandboxes.
# TYPE loam_sandboxes gauge
loam_sandboxes{state="paused"} 1
loam_sandboxes{state="running"} 2
`
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}

	c.Delete(map[string]string{"sandbox": "web"})
	b.Reset()
	r.Write(&b)
	if strings.Contains(b.String(), `sandbox="web"`) || !strings.Contains(b.String(), `we\"ird`) {
		t.Fatalf("after delete:\n%s", b.String())
	}
}
