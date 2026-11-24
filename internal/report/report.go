// Package report prints timelines as text, Markdown or JSON, and converts
// idle accelerator time into money.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/officialasishkumar/coldstart/internal/timeline"
)

// Price is what one accelerator costs per hour, in any currency.
type Price struct {
	PerGPUHour float64
	Currency   string
	GPUs       int // overrides the pod's GPU count when > 0
}

func (pr Price) gpus(t *timeline.Timeline) int {
	if pr.GPUs > 0 {
		return pr.GPUs
	}
	return t.GPUs
}

// Cost of one cold start's idle accelerator time.
func (pr Price) Cost(t *timeline.Timeline) float64 {
	return t.IdleGPU().Hours() * float64(pr.gpus(t)) * pr.PerGPUHour
}

// Dur prints a duration the way people read them: 3m12s, 4.2s, 180ms.
func Dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

const barWidth = 28

// Text writes one pod's timeline.
func Text(w io.Writer, t *timeline.Timeline, pr Price) {
	fmt.Fprintf(w, "pod %s/%s", t.Namespace, t.Pod)
	if t.Node != "" {
		fmt.Fprintf(w, " on %s", t.Node)
	}
	if g := pr.gpus(t); g > 0 {
		fmt.Fprintf(w, ", %d GPU", g)
		if g > 1 {
			fmt.Fprint(w, "s")
		}
	}
	fmt.Fprintln(w)
	total := t.Total()
	fmt.Fprintln(w)
	for _, p := range t.Phases {
		bar := 0
		if total > 0 {
			bar = int(float64(barWidth) * float64(p.Duration) / float64(total))
		}
		if bar == 0 && p.Duration > total/200 {
			bar = 1
		}
		name := p.Name
		if len(name) > 38 {
			name = name[:35] + "..."
		}
		fmt.Fprintf(w, "  %8s  %-38s %8s  %-*s", "+"+Dur(p.Start.Sub(t.Created)), name, Dur(p.Duration), barWidth, strings.Repeat("█", bar))
		if p.Note != "" {
			fmt.Fprintf(w, "  %s", p.Note)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	switch {
	case !t.FirstResponse.IsZero():
		fmt.Fprintf(w, "  created → first response  %s   (Ready at +%s)\n", Dur(total), Dur(t.Ready.Sub(t.Created)))
	case !t.Ready.IsZero():
		fmt.Fprintf(w, "  created → Ready  %s\n", Dur(total))
	default:
		fmt.Fprintf(w, "  not Ready after %s\n", Dur(total))
	}
	cats := categories(t)
	var parts []string
	for _, c := range cats {
		parts = append(parts, fmt.Sprintf("%s %s (%.0f%%)", c, Dur(t.ByCategory[c]), pct(t.ByCategory[c], total)))
	}
	fmt.Fprintf(w, "  where it went    %s\n", strings.Join(parts, ", "))
	var pulled int64
	cached := 0
	for _, p := range t.Pulls {
		pulled += p.Bytes
		if p.Cached {
			cached++
		}
	}
	if len(t.Pulls) > 0 {
		fmt.Fprintf(w, "  images           %d pulled", len(t.Pulls)-cached)
		if pulled > 0 {
			fmt.Fprintf(w, " (%s)", timeline.HumanBytes(pulled))
		}
		if cached > 0 {
			fmt.Fprintf(w, ", %d already on the node", cached)
		}
		fmt.Fprintln(w)
	}
	if g := pr.gpus(t); g > 0 && !t.Scheduled.IsZero() {
		fmt.Fprintf(w, "  idle accelerator %s × %d GPU", Dur(t.IdleGPU()), g)
		if pr.PerGPUHour > 0 {
			c := pr.Cost(t)
			fmt.Fprintf(w, " × %s%.2f/h = %s%.2f per cold start, %s%.0f per 1,000", pr.Currency, pr.PerGPUHour, pr.Currency, c, pr.Currency, c*1000)
		}
		fmt.Fprintln(w)
	}
	if b, ok := t.Biggest(); ok {
		fmt.Fprintf(w, "  biggest phase    %s, %s (%.0f%%)\n", b.Name, Dur(b.Duration), pct(b.Duration, total))
	}
	for _, p := range t.Problems {
		fmt.Fprintf(w, "  note             %s\n", p)
	}
}

func pct(d, total time.Duration) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(d) / float64(total)
}

func categories(t *timeline.Timeline) []string {
	var cats []string
	for c, d := range t.ByCategory {
		if d > 0 {
			cats = append(cats, c)
		}
	}
	sort.Slice(cats, func(i, j int) bool { return t.ByCategory[cats[i]] > t.ByCategory[cats[j]] })
	return cats
}

var tableCats = []string{timeline.CatSchedule, timeline.CatSetup, timeline.CatPull, timeline.CatInit, timeline.CatWarmup, timeline.CatRespond}

// Table writes many pods as one Markdown table plus medians.
func Table(w io.Writer, ts []*timeline.Timeline, pr Price) {
	head := []string{"pod", "node"}
	for _, c := range tableCats {
		head = append(head, c)
	}
	head = append(head, "total", "idle GPU")
	if pr.PerGPUHour > 0 {
		head = append(head, "cost")
	}
	fmt.Fprintf(w, "| %s |\n|%s\n", strings.Join(head, " | "), strings.Repeat("---|", len(head)))
	cols := map[string][]time.Duration{}
	var totals, idles []time.Duration
	var costs []float64
	for _, t := range ts {
		row := []string{t.Pod, t.Node}
		for _, c := range tableCats {
			row = append(row, Dur(t.ByCategory[c]))
			cols[c] = append(cols[c], t.ByCategory[c])
		}
		row = append(row, Dur(t.Total()), Dur(t.IdleGPU()))
		totals, idles = append(totals, t.Total()), append(idles, t.IdleGPU())
		if pr.PerGPUHour > 0 {
			c := pr.Cost(t)
			costs = append(costs, c)
			row = append(row, fmt.Sprintf("%s%.2f", pr.Currency, c))
		}
		if t.Ready.IsZero() {
			row[0] += " (not Ready)"
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(row, " | "))
	}
	if len(ts) > 1 {
		row := []string{"**median**", ""}
		for _, c := range tableCats {
			row = append(row, Dur(median(cols[c])))
		}
		row = append(row, Dur(median(totals)), Dur(median(idles)))
		if pr.PerGPUHour > 0 {
			slices.Sort(costs)
			row = append(row, fmt.Sprintf("%s%.2f", pr.Currency, costs[len(costs)/2]))
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(row, " | "))
	}
}

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}

// JSON writes the timelines with cost attached.
func JSON(w io.Writer, ts []*timeline.Timeline, pr Price) error {
	type out struct {
		*timeline.Timeline
		TotalNs   time.Duration `json:"totalNs"`
		IdleGPUNs time.Duration `json:"idleGpuNs"`
		Cost      float64       `json:"cost,omitempty"`
	}
	var rows []out
	for _, t := range ts {
		rows = append(rows, out{Timeline: t, TotalNs: t.Total(), IdleGPUNs: t.IdleGPU(), Cost: pr.Cost(t)})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
