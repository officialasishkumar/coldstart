package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Kubectl runs kubectl with fixed context, kubeconfig and namespace flags.
type Kubectl struct {
	Context    string
	Kubeconfig string
	Namespace  string
}

func (k Kubectl) base() []string {
	var a []string
	if k.Kubeconfig != "" {
		a = append(a, "--kubeconfig", k.Kubeconfig)
	}
	if k.Context != "" {
		a = append(a, "--context", k.Context)
	}
	if k.Namespace != "" {
		a = append(a, "--namespace", k.Namespace)
	}
	return a
}

// Run executes kubectl and returns stdout; stderr is folded into the error.
func (k Kubectl) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append(k.base(), args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), msg)
	}
	return out.Bytes(), nil
}

// Pod fetches one pod.
func (k Kubectl) Pod(ctx context.Context, name string) (*Pod, error) {
	b, err := k.Run(ctx, nil, "get", "pod", name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var p Pod
	return &p, json.Unmarshal(b, &p)
}

// Pods lists pods matching a label selector.
func (k Kubectl) Pods(ctx context.Context, selector string) ([]Pod, error) {
	args := []string{"get", "pods", "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	b, err := k.Run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var l PodList
	return l.Items, json.Unmarshal(b, &l)
}

// Events returns the events whose involved object is the named pod.
func (k Kubectl) Events(ctx context.Context, pod string) ([]Event, error) {
	b, err := k.Run(ctx, nil, "get", "events", "-o", "json",
		"--field-selector", "involvedObject.kind=Pod,involvedObject.name="+pod)
	if err != nil {
		return nil, err
	}
	var l EventList
	return l.Items, json.Unmarshal(b, &l)
}

// Create submits a manifest and returns the created pod's name. Manifests
// may use metadata.generateName so repeated runs get fresh pods.
func (k Kubectl) Create(ctx context.Context, manifest []byte) (string, error) {
	b, err := k.Run(ctx, manifest, "create", "-f", "-", "-o", "jsonpath={.kind}/{.metadata.name}")
	if err != nil {
		return "", err
	}
	kind, name, _ := strings.Cut(strings.TrimSpace(string(b)), "/")
	if kind != "Pod" {
		return "", fmt.Errorf("coldstart run takes a Pod manifest, got %s", kind)
	}
	return name, nil
}

// Delete removes a pod without waiting.
func (k Kubectl) Delete(ctx context.Context, pod string) error {
	_, err := k.Run(ctx, nil, "delete", "pod", pod, "--wait=false")
	return err
}

// Proxy sends a request through the API server's pod proxy, so no
// port-forward is needed. With a body it is a POST, otherwise a GET.
func (k Kubectl) Proxy(ctx context.Context, ns, pod string, port int, path string, body []byte) ([]byte, error) {
	uri := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/%s", ns, pod, port, strings.TrimPrefix(path, "/"))
	if body == nil {
		return k.Run(ctx, nil, "get", "--raw", uri)
	}
	return k.Run(ctx, body, "create", "--raw", uri, "-f", "-")
}
