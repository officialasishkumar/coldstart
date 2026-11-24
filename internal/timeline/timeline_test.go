package timeline

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/officialasishkumar/coldstart/internal/kube"
)

func load(t *testing.T) (*kube.Pod, []kube.Event) {
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
	return &p, el.Items
}

// The fixture is a real kind run: curl init container downloading a GGUF
// model, then a llama.cpp server image pulled cold.
func TestBuildRealPod(t *testing.T) {
	p, ev := load(t)
	tl := Build(p, ev, time.Time{})
	if tl.Ready.IsZero() {
		t.Fatal("fixture pod should be Ready")
	}
	if len(tl.Pulls) != 2 {
		t.Fatalf("pulls = %d, want 2", len(tl.Pulls))
	}
	srv := tl.Pulls[1]
	if srv.Container != "server" || srv.Bytes != 291965819 || srv.Pull != 49427*time.Millisecond {
		t.Errorf("server pull = %+v", srv)
	}
	var sum time.Duration
	for i, ph := range tl.Phases {
		sum += ph.Duration
		if i > 0 && ph.Start.Before(tl.Phases[i-1].End) {
			t.Errorf("phase %q starts before previous ends", ph.Name)
		}
	}
	if sum != tl.Total() {
		t.Errorf("phases sum to %v, total is %v", sum, tl.Total())
	}
	if b, _ := tl.Biggest(); b.Category != CatInit && b.Category != CatPull {
		t.Errorf("biggest = %+v", b)
	}
}

func ts(sec float64) time.Time {
	return time.Date(2027, 3, 15, 9, 0, 0, 0, time.UTC).Add(time.Duration(sec * float64(time.Second)))
}

func TestGapsAndZeroLengthPhases(t *testing.T) {
	var p kube.Pod
	p.Metadata.Name = "x"
	p.Metadata.CreationTimestamp = ts(0)
	p.Status.Conditions = []kube.Condition{{Type: "PodScheduled", Status: "True", LastTransitionTime: ts(0)}, {Type: "Ready", Status: "True", LastTransitionTime: ts(2)}}
	p.Spec.InitContainers = []kube.Container{{Name: "fetch"}}
	var initSt kube.ContainerStatus
	initSt.Name = "fetch"
	initSt.State.Terminated = &struct {
		StartedAt  time.Time `json:"startedAt"`
		FinishedAt time.Time `json:"finishedAt"`
		ExitCode   int       `json:"exitCode"`
		Reason     string    `json:"reason"`
	}{StartedAt: ts(1), FinishedAt: ts(1)}
	var main kube.ContainerStatus
	main.Name = "app"
	main.State.Running = &struct {
		StartedAt time.Time `json:"startedAt"`
	}{StartedAt: ts(1)}
	p.Status.InitContainerStatuses = []kube.ContainerStatus{initSt}
	p.Status.ContainerStatuses = []kube.ContainerStatus{main}

	tl := Build(&p, nil, ts(2.5))
	var sum time.Duration
	for i, ph := range tl.Phases {
		sum += ph.Duration
		if i > 0 && ph.Start.Before(tl.Phases[i-1].End) {
			t.Errorf("phase %d %q starts at %v before previous end %v", i, ph.Name, ph.Start, tl.Phases[i-1].End)
		}
	}
	if sum != 2500*time.Millisecond {
		t.Errorf("sum = %v, want 2.5s", sum)
	}
	if tl.IdleGPU() != 2500*time.Millisecond {
		t.Errorf("idle = %v", tl.IdleGPU())
	}
}

func TestParsePullMessages(t *testing.T) {
	ev := func(reason, msg, c string, at float64) kube.Event {
		var e kube.Event
		e.Reason, e.Message = reason, msg
		e.InvolvedObject.FieldPath = "spec.containers{" + c + "}"
		w := ts(at)
		e.FirstTimestamp = &w
		return e
	}
	pulls := ParsePulls([]kube.Event{
		ev("Pulling", `Pulling image "vllm/vllm-openai:v0.9.0"`, "vllm", 3),
		ev("Pulled", `Successfully pulled image "vllm/vllm-openai:v0.9.0" in 2m3.5s (2m10.25s including waiting). Image size: 9876543210 bytes.`, "vllm", 134),
		ev("Pulled", `Container image "busybox:1.36" already present on machine`, "side", 1),
		// Older kubelets: no size, no "including waiting".
		ev("Pulled", `Successfully pulled image "nginx" in 812ms`, "old", 5),
	})
	if len(pulls) != 3 {
		t.Fatalf("got %d pulls: %+v", len(pulls), pulls)
	}
	byC := map[string]Pull{}
	for _, p := range pulls {
		byC[p.Container] = p
	}
	v := byC["vllm"]
	if v.Pull != 123500*time.Millisecond || v.Waiting != 130250*time.Millisecond || v.Bytes != 9876543210 || !v.End.Equal(ts(3).Add(v.Waiting)) {
		t.Errorf("vllm = %+v", v)
	}
	if !byC["side"].Cached {
		t.Error("side should be cached")
	}
	if o := byC["old"]; o.Pull != 812*time.Millisecond || !o.Start.Equal(ts(5).Add(-812*time.Millisecond)) {
		t.Errorf("old = %+v", o)
	}
}

func TestGPUs(t *testing.T) {
	var p kube.Pod
	c := func(name, gpus string, always bool) kube.Container {
		k := kube.Container{Name: name}
		k.Resources.Limits = map[string]string{"nvidia.com/gpu": gpus}
		if always {
			k.RestartPolicy = "Always"
		}
		return k
	}
	p.Spec.Containers = []kube.Container{c("a", "2", false), c("b", "1", false)}
	p.Spec.InitContainers = []kube.Container{c("warm", "4", false)}
	if g := GPUs(&p); g != 4 {
		t.Errorf("GPUs = %d, want 4 (init container max wins)", g)
	}
	p.Spec.InitContainers = []kube.Container{c("side", "1", true)}
	if g := GPUs(&p); g != 4 {
		t.Errorf("GPUs = %d, want 4 (sidecar adds)", g)
	}
}
