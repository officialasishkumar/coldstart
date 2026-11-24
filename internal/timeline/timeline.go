// Package timeline turns a pod's status and events into a startup timeline:
// a gap-free sequence of phases from creation to readiness (and, when
// measured, the first successful response), each with a category.
package timeline

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/officialasishkumar/coldstart/internal/kube"
)

// Categories a phase can belong to. Every second between creation and the
// end of the timeline is counted in exactly one of them.
const (
	CatSchedule = "schedule"   // waiting for a node (includes node provisioning)
	CatSetup    = "setup"      // sandbox, network, volume mounts, container create
	CatPull     = "image pull" // fetching container images
	CatInit     = "init"       // init containers running (model downloads usually live here)
	CatWarmup   = "warm-up"    // main container started, not yet Ready (model load, readiness probe)
	CatRespond  = "first response"
)

// Phase is one segment of the timeline.
type Phase struct {
	Category  string        `json:"category"`
	Name      string        `json:"name"`
	Container string        `json:"container,omitempty"`
	Start     time.Time     `json:"start"`
	End       time.Time     `json:"end"`
	Duration  time.Duration `json:"durationNs"`
	Note      string        `json:"note,omitempty"`
}

// Pull is one image pull as the kubelet reported it.
type Pull struct {
	Container string        `json:"container"`
	Image     string        `json:"image"`
	Start     time.Time     `json:"start"`
	End       time.Time     `json:"end"`
	Pull      time.Duration `json:"pullNs"`    // time spent pulling
	Waiting   time.Duration `json:"waitingNs"` // including time queued behind other pulls
	Bytes     int64         `json:"bytes,omitempty"`
	Cached    bool          `json:"cached"`
}

// Timeline is the result for one pod.
type Timeline struct {
	Pod           string                   `json:"pod"`
	Namespace     string                   `json:"namespace"`
	Node          string                   `json:"node"`
	Created       time.Time                `json:"created"`
	Scheduled     time.Time                `json:"scheduled,omitzero"`
	Ready         time.Time                `json:"ready,omitzero"`
	FirstResponse time.Time                `json:"firstResponse,omitzero"`
	Phases        []Phase                  `json:"phases"`
	Pulls         []Pull                   `json:"pulls"`
	GPUs          int                      `json:"gpus"`
	Restarts      int                      `json:"restarts"`
	Problems      []string                 `json:"problems,omitempty"`
	ByCategory    map[string]time.Duration `json:"byCategoryNs"`
}

// End is the last instant the timeline covers.
func (t *Timeline) End() time.Time {
	switch {
	case !t.FirstResponse.IsZero():
		return t.FirstResponse
	case !t.Ready.IsZero():
		return t.Ready
	}
	if n := len(t.Phases); n > 0 {
		return t.Phases[n-1].End
	}
	return t.Created
}

// Total is creation to End.
func (t *Timeline) Total() time.Duration { return t.End().Sub(t.Created) }

// IdleGPU is the time a scheduled pod holds its accelerators before it can
// serve: from binding to the node until Ready (or the first response).
func (t *Timeline) IdleGPU() time.Duration {
	if t.Scheduled.IsZero() {
		return 0
	}
	return t.End().Sub(t.Scheduled)
}

// Biggest returns the longest phase.
func (t *Timeline) Biggest() (Phase, bool) {
	var best Phase
	for _, p := range t.Phases {
		if p.Duration > best.Duration {
			best = p
		}
	}
	return best, best.Duration > 0
}

var (
	fieldPathRe = regexp.MustCompile(`^spec\.(initContainers|containers)\{([^}]+)\}$`)
	pulledRe    = regexp.MustCompile(`Successfully pulled image "([^"]+)" in ([0-9][0-9a-zµ.]*?)\.?(?:\s|$)`)
	waitingRe   = regexp.MustCompile(`\(([0-9][0-9a-zµ.]*) including waiting\)`)
	sizeRe      = regexp.MustCompile(`Image size: (\d+) bytes`)
	presentRe   = regexp.MustCompile(`image "([^"]+)" already present on machine`)
)

func containerOf(e kube.Event) string {
	if m := fieldPathRe.FindStringSubmatch(e.InvolvedObject.FieldPath); m != nil {
		return m[2]
	}
	return ""
}

// ParsePulls extracts one pull per container from kubelet events: the first
// Pulling and the first Pulled (or "already present") after it.
func ParsePulls(events []kube.Event) []Pull {
	sorted := append([]kube.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].When().Before(sorted[j].When()) })
	pulling := map[string]kube.Event{}
	done := map[string]bool{}
	var out []Pull
	for _, e := range sorted {
		c := containerOf(e)
		if c == "" || done[c] {
			continue
		}
		switch e.Reason {
		case "Pulling":
			if _, ok := pulling[c]; !ok {
				pulling[c] = e
			}
		case "Pulled":
			p := Pull{Container: c, End: e.When()}
			if m := presentRe.FindStringSubmatch(e.Message); m != nil {
				p.Image, p.Cached, p.Start = m[1], true, e.When()
			} else if m := pulledRe.FindStringSubmatch(e.Message); m != nil {
				p.Image = m[1]
				p.Pull, _ = time.ParseDuration(m[2])
				p.Waiting = p.Pull
				if w := waitingRe.FindStringSubmatch(e.Message); w != nil {
					if d, err := time.ParseDuration(w[1]); err == nil {
						p.Waiting = d
					}
				}
				if s := sizeRe.FindStringSubmatch(e.Message); s != nil {
					p.Bytes, _ = strconv.ParseInt(s[1], 10, 64)
				}
				if start, ok := pulling[c]; ok {
					// The Pulling event marks the request; the pull ends
					// "including waiting" later. Pulled timestamps are often
					// whole seconds, so this is the more precise end.
					p.Start = start.When()
					p.End = p.Start.Add(p.Waiting)
				} else {
					p.Start = p.End.Add(-p.Waiting)
				}
			} else {
				continue
			}
			done[c] = true
			out = append(out, p)
		}
	}
	return out
}

// GPUs counts accelerators the pod requests, using the Kubernetes rule for
// effective requests: max(largest init container, sum of app containers).
func GPUs(p *kube.Pod) int {
	count := func(c kube.Container) int {
		n := 0
		for _, m := range []map[string]string{c.Resources.Limits, c.Resources.Requests} {
			for k, v := range m {
				if strings.Contains(strings.ToLower(k), "gpu") {
					if q, err := strconv.Atoi(v); err == nil && q > n {
						n = q
					}
				}
			}
		}
		return n
	}
	sum, initMax := 0, 0
	for _, c := range p.Spec.Containers {
		sum += count(c)
	}
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy == "Always" {
			sum += count(c)
		} else if n := count(c); n > initMax {
			initMax = n
		}
	}
	return max(sum, initMax)
}

type segment struct {
	cat, name, container, note string
	start, end                 time.Time
}

// Build assembles the timeline. firstResponse may be zero.
func Build(p *kube.Pod, events []kube.Event, firstResponse time.Time) *Timeline {
	t := &Timeline{
		Pod: p.Metadata.Name, Namespace: p.Metadata.Namespace, Node: p.Spec.NodeName,
		Created: p.Metadata.CreationTimestamp, FirstResponse: firstResponse,
		GPUs: GPUs(p), ByCategory: map[string]time.Duration{},
	}
	t.Scheduled, _ = p.Condition("PodScheduled")
	if ready, ok := p.Condition("Ready"); ok {
		t.Ready = ready
	}
	t.Pulls = ParsePulls(events)

	var segs []segment
	if !t.Scheduled.IsZero() {
		note := ""
		for _, e := range events {
			if e.Reason == "FailedScheduling" {
				note = "waited for capacity: " + firstLine(e.Message)
				break
			}
		}
		segs = append(segs, segment{cat: CatSchedule, name: "schedule", note: note, start: t.Created, end: t.Scheduled})
	}
	for _, pl := range t.Pulls {
		if pl.Cached {
			continue
		}
		note := ""
		if pl.Bytes > 0 && pl.Pull > 0 {
			note = humanBytes(pl.Bytes) + " at " + humanBytes(int64(float64(pl.Bytes)/pl.Pull.Seconds())) + "/s"
		}
		segs = append(segs, segment{cat: CatPull, name: "pull " + shortImage(pl.Image), container: pl.Container, note: note, start: pl.Start, end: pl.End})
	}
	var lastMainStart time.Time
	initSpec := map[string]kube.Container{}
	for _, c := range p.Spec.InitContainers {
		initSpec[c.Name] = c
	}
	for _, s := range p.Status.InitContainerStatuses {
		t.Restarts += s.RestartCount
		if initSpec[s.Name].RestartPolicy == "Always" {
			continue // sidecars run alongside the app; their start is setup
		}
		term := s.State.Terminated
		if term == nil && s.LastState.Terminated != nil && s.State.Running != nil {
			term = s.LastState.Terminated
		}
		if term != nil {
			note := ""
			if term.ExitCode != 0 {
				note = "exit " + strconv.Itoa(term.ExitCode)
			}
			segs = append(segs, segment{cat: CatInit, name: "init " + s.Name, container: s.Name, note: note, start: term.StartedAt, end: term.FinishedAt})
		} else if s.State.Running != nil && t.Ready.IsZero() {
			t.Problems = append(t.Problems, "init container "+s.Name+" still running")
		} else if s.State.Waiting != nil {
			t.Problems = append(t.Problems, "init container "+s.Name+" waiting: "+s.State.Waiting.Reason)
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		t.Restarts += s.RestartCount
		var started time.Time
		switch {
		case s.State.Running != nil:
			started = s.State.Running.StartedAt
		case s.State.Terminated != nil:
			started = s.State.Terminated.StartedAt
		}
		if s.RestartCount > 0 && s.LastState.Terminated != nil && s.LastState.Terminated.StartedAt.Before(started) {
			t.Problems = append(t.Problems, s.Name+" restarted "+strconv.Itoa(s.RestartCount)+"x before becoming ready (last exit "+strconv.Itoa(s.LastState.Terminated.ExitCode)+")")
		}
		if s.State.Waiting != nil {
			t.Problems = append(t.Problems, s.Name+" waiting: "+s.State.Waiting.Reason)
		}
		if started.After(lastMainStart) {
			lastMainStart = started
		}
	}
	if !lastMainStart.IsZero() && !t.Ready.IsZero() {
		segs = append(segs, segment{cat: CatWarmup, name: "warm-up until Ready", start: lastMainStart, end: t.Ready})
	} else if t.Ready.IsZero() && t.Problems == nil {
		t.Problems = append(t.Problems, "pod is not Ready")
	}
	if !firstResponse.IsZero() && !t.Ready.IsZero() {
		segs = append(segs, segment{cat: CatRespond, name: "first response", start: t.Ready, end: firstResponse})
	}

	// Lay the segments on one line: overlaps go to whichever started first,
	// gaps become setup.
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].start.Before(segs[j].start) })
	cursor := t.Created
	sawWork := false
	for _, s := range segs {
		start := s.start
		if start.Before(cursor) {
			start = cursor
		}
		if gap := start.Sub(cursor); gap > 0 {
			name := "kubelet: create/start container"
			if !sawWork {
				name = "kubelet: sandbox, network, volumes"
			}
			t.add(Phase{Category: CatSetup, Name: name, Start: cursor, End: start})
			cursor = start
		}
		if s.cat != CatSchedule {
			sawWork = true
		}
		if s.end.After(start) {
			t.add(Phase{Category: s.cat, Name: s.name, Container: s.container, Note: s.note, Start: start, End: s.end})
			cursor = s.end
		} else if s.cat == CatPull || s.cat == CatInit {
			t.add(Phase{Category: s.cat, Name: s.name, Container: s.container, Note: s.note, Start: start, End: start})
		}
	}
	if end := t.End(); end.After(cursor) && (!t.Ready.IsZero()) {
		t.add(Phase{Category: CatSetup, Name: "kubelet: create/start container", Start: cursor, End: end})
	}
	return t
}

func (t *Timeline) add(p Phase) {
	p.Duration = p.End.Sub(p.Start)
	t.Phases = append(t.Phases, p)
	t.ByCategory[p.Category] += p.Duration
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 90 {
		s = s[:87] + "..."
	}
	return s
}

func shortImage(img string) string {
	if i := strings.LastIndex(img, "/"); i >= 0 {
		img = img[i+1:]
	}
	if at := strings.Index(img, "@sha256:"); at >= 0 {
		img = img[:at]
	}
	return img
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("kMGTPE"[exp]) + "B"
}

// HumanBytes is exported for the report package.
func HumanBytes(n int64) string { return humanBytes(n) }
