package report

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/officialasishkumar/coldstart/internal/kube"
	"github.com/officialasishkumar/coldstart/internal/timeline"
)

func fixture(t *testing.T) *timeline.Timeline {
	t.Helper()
	var p kube.Pod
	var el kube.EventList
	for path, v := range map[string]any{"../../testdata/llama-pod.json": &p, "../../testdata/llama-events.json": &el} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	return timeline.Build(&p, el.Items, time.Time{})
}

func TestCost(t *testing.T) {
	tl := fixture(t)
	pr := Price{PerGPUHour: 3600, GPUs: 2} // one unit per GPU-second
	want := tl.IdleGPU().Seconds() * 2
	if got := pr.Cost(tl); got < want-1e-6 || got > want+1e-6 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestTextMentionsEveryPhase(t *testing.T) {
	tl := fixture(t)
	var b bytes.Buffer
	Text(&b, tl, Price{PerGPUHour: 2.49, Currency: "$", GPUs: 1})
	out := b.String()
	for _, want := range []string{"pull llama.cpp:server", "init fetch-weights", "292.0 MB", "per cold start", "biggest phase"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestTableMedian(t *testing.T) {
	a, b, c := fixture(t), fixture(t), fixture(t)
	var buf bytes.Buffer
	Table(&buf, []*timeline.Timeline{a, b, c}, Price{})
	if !strings.Contains(buf.String(), "**median**") {
		t.Errorf("no median row:\n%s", buf.String())
	}
}

func TestDur(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 180 * time.Millisecond: "180ms", 4200 * time.Millisecond: "4.2s", 192 * time.Second: "3m12s", 2*time.Hour + 5*time.Minute: "2h05m"} {
		if got := Dur(d); got != want {
			t.Errorf("Dur(%v) = %q, want %q", d, got, want)
		}
	}
}
