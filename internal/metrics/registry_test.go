package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestCounterExposition(t *testing.T) {
	tests := []struct {
		name string
		do   func(c *Counter)
		want []string
	}{
		{
			name: "no labels",
			do:   func(c *Counter) { c.Inc(nil); c.Add(2, nil) },
			want: []string{
				"# HELP gotochanger_test_total help text",
				"# TYPE gotochanger_test_total counter",
				"gotochanger_test_total 3",
			},
		},
		{
			name: "labels sorted regardless of insertion order",
			do: func(c *Counter) {
				c.Inc(map[string]string{"b": "2", "a": "1"})
			},
			want: []string{
				`gotochanger_test_total{a="1",b="2"} 1`,
			},
		},
		{
			name: "distinct label sets accumulate independently",
			do: func(c *Counter) {
				c.Inc(map[string]string{"op": "load"})
				c.Inc(map[string]string{"op": "load"})
				c.Inc(map[string]string{"op": "unload"})
			},
			want: []string{
				`gotochanger_test_total{op="load"} 2`,
				`gotochanger_test_total{op="unload"} 1`,
			},
		},
		{
			name: "label value escaping",
			do: func(c *Counter) {
				c.Inc(map[string]string{"path": `a"b\c` + "\n" + "d"})
			},
			want: []string{
				`gotochanger_test_total{path="a\"b\\c\nd"} 1`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry()
			c := reg.NewCounter("gotochanger_test_total", "help text")
			tt.do(c)

			var buf bytes.Buffer
			if err := reg.WriteExposition(&buf); err != nil {
				t.Fatalf("WriteExposition: %v", err)
			}
			out := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("exposition missing %q, got:\n%s", want, out)
				}
			}
		})
	}
}

func TestCounterRejectsNegativeAdd(t *testing.T) {
	reg := NewRegistry()
	c := reg.NewCounter("gotochanger_test_total", "help")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on negative Add")
		}
	}()
	c.Add(-1, nil)
}

func TestGaugeSetAndReset(t *testing.T) {
	reg := NewRegistry()
	g := reg.NewGauge("gotochanger_test_gauge", "help")

	g.Set(24, nil)
	var buf bytes.Buffer
	if err := reg.WriteExposition(&buf); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	if !strings.Contains(buf.String(), "gotochanger_test_gauge 24\n") {
		t.Fatalf("expected gauge value 24, got:\n%s", buf.String())
	}

	g.Set(5, map[string]string{"status": "in_slot"})
	g.Set(2, map[string]string{"status": "in_drive"})
	g.Reset()
	g.Set(9, map[string]string{"status": "in_slot"})

	buf.Reset()
	if err := reg.WriteExposition(&buf); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, `status="in_drive"`) {
		t.Fatalf("Reset should have dropped the stale in_drive sample, got:\n%s", out)
	}
	if !strings.Contains(out, `gotochanger_test_gauge{status="in_slot"} 9`) {
		t.Fatalf("expected refreshed in_slot=9 sample, got:\n%s", out)
	}
}

func TestGaugeDelete(t *testing.T) {
	reg := NewRegistry()
	g := reg.NewGauge("gotochanger_test_gauge", "help")
	g.Set(1700000000, nil)
	g.Delete(nil)

	var buf bytes.Buffer
	if err := reg.WriteExposition(&buf); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	out := buf.String()
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "#") && line != "" {
			t.Fatalf("expected deleted sample to be omitted entirely, got sample line %q in:\n%s", line, out)
		}
	}
	if !strings.Contains(out, "# HELP gotochanger_test_gauge") {
		t.Fatalf("HELP/TYPE header should still be present even with no samples, got:\n%s", out)
	}
}

func TestHistogramBucketsSumCount(t *testing.T) {
	reg := NewRegistry()
	h := reg.NewHistogram("gotochanger_test_duration_seconds", "help", []float64{0.1, 0.5, 1})

	h.Observe(0.05, map[string]string{"op": "load"})
	h.Observe(0.2, map[string]string{"op": "load"})
	h.Observe(2, map[string]string{"op": "load"})

	var buf bytes.Buffer
	if err := reg.WriteExposition(&buf); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`gotochanger_test_duration_seconds_bucket{le="0.1",op="load"} 1`,
		`gotochanger_test_duration_seconds_bucket{le="0.5",op="load"} 2`,
		`gotochanger_test_duration_seconds_bucket{le="1",op="load"} 2`,
		`gotochanger_test_duration_seconds_bucket{le="+Inf",op="load"} 3`,
		`gotochanger_test_duration_seconds_sum{op="load"} 2.25`,
		`gotochanger_test_duration_seconds_count{op="load"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q, got:\n%s", want, out)
		}
	}
}

func TestDuplicateNamePanics(t *testing.T) {
	reg := NewRegistry()
	reg.NewCounter("gotochanger_dup", "help")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate metric name")
		}
	}()
	reg.NewGauge("gotochanger_dup", "help")
}

// TestConcurrentIncrements exercises Counter/Gauge/Histogram under
// concurrent access from multiple goroutines - run with -race, this is
// what actually catches an unprotected map mutation, not the assertion
// below (which only checks the final count is correct).
func TestConcurrentIncrements(t *testing.T) {
	reg := NewRegistry()
	counter := reg.NewCounter("gotochanger_test_total", "help")
	gauge := reg.NewGauge("gotochanger_test_gauge", "help")
	hist := reg.NewHistogram("gotochanger_test_duration_seconds", "help", []float64{0.1, 1})

	const goroutines = 50
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				counter.Inc(map[string]string{"op": "load"})
				gauge.Set(float64(j), map[string]string{"worker": "w"})
				hist.Observe(0.05, map[string]string{"op": "load"})
			}
		}(i)
	}
	wg.Wait()

	var buf bytes.Buffer
	if err := reg.WriteExposition(&buf); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	out := buf.String()
	want := `gotochanger_test_total{op="load"} 5000`
	if !strings.Contains(out, want) {
		t.Errorf("exposition missing %q, got:\n%s", want, out)
	}
	wantCount := `gotochanger_test_duration_seconds_count{op="load"} 5000`
	if !strings.Contains(out, wantCount) {
		t.Errorf("exposition missing %q, got:\n%s", wantCount, out)
	}
}
