// coldstart shows where a Kubernetes pod's startup time goes (scheduling,
// image pulls, init containers, warm-up, first response) and what the idle
// accelerator time costs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/officialasishkumar/coldstart/internal/kube"
	"github.com/officialasishkumar/coldstart/internal/report"
	"github.com/officialasishkumar/coldstart/internal/timeline"
)

var version = "dev"

const usage = `coldstart: where does a pod's startup time go, and what does it cost?

Usage:
  coldstart pod NAME [flags]            timeline of one existing pod
  coldstart pods [-l SELECTOR] [flags]  table of many pods, with medians
  coldstart run -f POD.yaml [flags]     create a pod, wait for it, measure it
  coldstart file POD.json EVENTS.json   timeline from saved kubectl output
  coldstart version

Cluster flags (all commands that talk to a cluster):
  --context NAME      kubeconfig context (default: kubectl's current context)
  --kubeconfig FILE   kubeconfig file
  -n NAMESPACE        namespace

Cost flags:
  --gpu-hour PRICE    price of one accelerator-hour; turns idle time into money
  --currency SYMBOL   printed before prices (default $)
  --gpus N            accelerators per pod (default: read from the pod's resources)

Output flags:
  --json FILE         also write the timelines as JSON ("-" for stdout only)

Flags for run:
  -f FILE             Pod manifest (YAML or JSON); use metadata.generateName to repeat runs
  --probe-port N      after Ready, poll this container port through the API server proxy
  --probe-path PATH   path to poll (default /)
  --probe-body FILE   POST this file instead of GET (e.g. a one-token completion request)
  --timeout DUR       give up after this long (default 30m)
  --delete            delete the pod afterwards

Example:
  coldstart run -f vllm-pod.yaml --gpu-hour 2.49 --probe-port 8000 \
      --probe-path /v1/completions --probe-body one-token.json --delete
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "coldstart:", err)
		os.Exit(1)
	}
}

type common struct {
	kc      kube.Kubectl
	price   report.Price
	jsonOut string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.kc.Context, "context", "", "kubeconfig context")
	fs.StringVar(&c.kc.Kubeconfig, "kubeconfig", "", "kubeconfig file")
	fs.StringVar(&c.kc.Namespace, "n", "", "namespace")
	fs.StringVar(&c.kc.Namespace, "namespace", "", "namespace")
	fs.Float64Var(&c.price.PerGPUHour, "gpu-hour", 0, "price per accelerator-hour")
	fs.StringVar(&c.price.Currency, "currency", "$", "currency symbol")
	fs.IntVar(&c.price.GPUs, "gpus", 0, "accelerators per pod")
	fs.StringVar(&c.jsonOut, "json", "", "write JSON here")
}

func (c *common) emit(ts []*timeline.Timeline, table bool) error {
	if c.jsonOut != "-" {
		if table {
			report.Table(os.Stdout, ts, c.price)
		} else {
			for i, t := range ts {
				if i > 0 {
					fmt.Println()
				}
				report.Text(os.Stdout, t, c.price)
			}
		}
	}
	if c.jsonOut == "" {
		return nil
	}
	if c.jsonOut == "-" {
		return report.JSON(os.Stdout, ts, c.price)
	}
	f, err := os.Create(c.jsonOut)
	if err != nil {
		return err
	}
	defer f.Close()
	return report.JSON(f, ts, c.price)
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "pod":
		return cmdPod(ctx, args[1:])
	case "pods":
		return cmdPods(ctx, args[1:])
	case "run":
		return cmdRun(ctx, args[1:])
	case "file":
		return cmdFile(args[1:])
	case "version":
		fmt.Println("coldstart", version)
		return nil
	}
	return fmt.Errorf("unknown command %q (try --help)", args[0])
}

func cmdPod(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pod", flag.ContinueOnError)
	var c common
	c.register(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: coldstart pod NAME [flags]")
	}
	t, err := measure(ctx, c.kc, pos[0], time.Time{})
	if err != nil {
		return err
	}
	return c.emit([]*timeline.Timeline{t}, false)
}

func cmdPods(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pods", flag.ContinueOnError)
	var c common
	c.register(fs)
	selector := fs.String("l", "", "label selector")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}
	pods, err := c.kc.Pods(ctx, *selector)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return errors.New("no pods matched")
	}
	var ts []*timeline.Timeline
	for i := range pods {
		ev, err := c.kc.Events(ctx, pods[i].Metadata.Name)
		if err != nil {
			return err
		}
		ts = append(ts, timeline.Build(&pods[i], ev, time.Time{}))
	}
	return c.emit(ts, true)
}

func measure(ctx context.Context, kc kube.Kubectl, name string, firstResponse time.Time) (*timeline.Timeline, error) {
	p, err := kc.Pod(ctx, name)
	if err != nil {
		return nil, err
	}
	ev, err := kc.Events(ctx, name)
	if err != nil {
		return nil, err
	}
	return timeline.Build(p, ev, firstResponse), nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var c common
	c.register(fs)
	file := fs.String("f", "", "Pod manifest")
	port := fs.Int("probe-port", 0, "port to probe after Ready")
	path := fs.String("probe-path", "/", "path to probe")
	bodyFile := fs.String("probe-body", "", "POST this file")
	timeout := fs.Duration("timeout", 30*time.Minute, "give up after")
	del := fs.Bool("delete", false, "delete the pod afterwards")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("usage: coldstart run -f POD.yaml [flags]")
	}
	manifest, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var body []byte
	if *bodyFile != "" {
		if body, err = os.ReadFile(*bodyFile); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	name, err := c.kc.Create(ctx, manifest)
	if err != nil {
		return err
	}
	if *del {
		defer func() {
			if err := c.kc.Delete(context.Background(), name); err != nil {
				fmt.Fprintln(os.Stderr, "coldstart:", err)
			}
		}()
	}
	fmt.Fprintf(os.Stderr, "created pod %s, waiting for Ready", name)
	p, err := waitReady(ctx, c.kc, name)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		// Show how far it got; the timeline's notes usually say why.
		if t, merr := measure(context.Background(), c.kc, name, time.Time{}); merr == nil {
			_ = c.emit([]*timeline.Timeline{t}, false)
		}
		return err
	}
	var first time.Time
	if *port > 0 {
		fmt.Fprintf(os.Stderr, "Ready; probing port %d%s", *port, *path)
		first, err = probe(ctx, c.kc, p.Metadata.Namespace, name, *port, *path, body)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
	}
	t, err := measure(context.Background(), c.kc, name, first)
	if err != nil {
		return err
	}
	return c.emit([]*timeline.Timeline{t}, false)
}

func waitReady(ctx context.Context, kc kube.Kubectl, name string) (*kube.Pod, error) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for n := 0; ; n++ {
		p, err := kc.Pod(ctx, name)
		if err == nil {
			if _, ok := p.Condition("Ready"); ok {
				return p, nil
			}
			if p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded" {
				return nil, fmt.Errorf("pod %s ended in phase %s before becoming Ready", name, p.Status.Phase)
			}
		}
		if n%10 == 9 {
			fmt.Fprint(os.Stderr, ".")
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("pod %s not Ready: %w", name, ctx.Err())
		case <-tick.C:
		}
	}
}

// probe polls until the endpoint answers without error and returns when it
// first did. Readiness probes often pass before a model server can answer a
// real request, so this is the honest end of a cold start.
func probe(ctx context.Context, kc kube.Kubectl, ns, pod string, port int, path string, body []byte) (time.Time, error) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, err := kc.Proxy(reqCtx, ns, pod, port, path, body)
		cancel()
		if err == nil {
			return time.Now(), nil
		}
		select {
		case <-ctx.Done():
			return time.Time{}, fmt.Errorf("no successful response: %w (last error: %v)", ctx.Err(), err)
		case <-tick.C:
		}
	}
}

func cmdFile(args []string) error {
	fs := flag.NewFlagSet("file", flag.ContinueOnError)
	var c common
	c.register(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: coldstart file POD.json EVENTS.json   (from kubectl get pod NAME -o json / kubectl get events -o json)")
	}
	var p kube.Pod
	if err := readJSON(pos[0], &p); err != nil {
		return err
	}
	var el kube.EventList
	if err := readJSON(pos[1], &el); err != nil {
		return err
	}
	var mine []kube.Event
	for _, e := range el.Items {
		if e.InvolvedObject.Kind == "Pod" && e.InvolvedObject.Name == p.Metadata.Name {
			mine = append(mine, e)
		}
	}
	return c.emit([]*timeline.Timeline{timeline.Build(&p, mine, time.Time{})}, false)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// parseInterspersed lets flags appear after positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}
