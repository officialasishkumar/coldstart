# coldstart

Where does a Kubernetes pod's startup time go, and what does the idle
accelerator cost while it waits?

A GPU node is billed from the moment a pod is bound to it, but an inference
pod cannot answer a request until its images are pulled, its weights are
downloaded and its server has warmed up. `coldstart` reads the pod's status
and the kubelet's events and lays every second from creation to the first
successful response on one line, with no overlaps and no gaps:

```
$ coldstart run -f examples/llama-cpp-pod.yaml --probe-port 8080 \
    --probe-path /v1/completions --probe-body examples/one-token.json \
    --gpus 1 --gpu-hour 2.49
created pod llm-coldstart-j44tp, waiting for Ready...........
Ready; probing port 8080/v1/completions
pod default/llm-coldstart-j44tp on coldstart-control-plane, 1 GPU

       +0s  pull curl:8.11.1                           7.3s  █                             9.9 MB at 1.3 MB/s
     +7.3s  kubelet: create/start container           675ms  █
     +8.0s  init fetch-weights                        52.0s  ████████████
    +1m00s  pull llama.cpp:server                     49.4s  ████████████                  292.0 MB at 5.9 MB/s
    +1m49s  kubelet: create/start container           573ms  █
    +1m50s  warm-up until Ready                        1.0s  █
    +1m51s  first response                             1.3s  █

  created → first response  1m52s   (Ready at +1m51s)
  where it went    image pull 56.8s (51%), init 52.0s (46%), first response 1.3s (1%), setup 1.2s (1%), warm-up 1.0s (1%)
  images           2 pulled (301.8 MB)
  idle accelerator 1m52s × 1 GPU × $2.49/h = $0.08 per cold start, $78 per 1,000
  biggest phase    init fetch-weights, 52.0s (46%)
```

That run is real: a [kind](https://kind.sigs.k8s.io) cluster on a laptop, a
curl init container downloading a 0.5B-parameter GGUF model from Hugging
Face, and the llama.cpp server image pulled cold. There is no GPU in it; `--gpus
1 --gpu-hour 2.49` prices the wait as if the pod held one accelerator at a
typical on-demand rate, which is what the tool is for.

## What the fixes are worth

The same model server, three ways, measured with the commands in this README
on the same cluster (24 Nov 2026):

| setup | images | weights | created → first response | idle GPU per 1,000 starts at $2.49/h |
|---|---|---|---|---|
| cold node, weights downloaded at start | pulled (302 MB) | downloaded | **1m52s** | $78 |
| warm images, weights downloaded at start | cached | downloaded | **45.9s** | $32 |
| warm images, node-local weight cache ([example](examples/llama-cpp-pod-node-cache.yaml)) | cached | on the node | **2.9s** | $2 |

A small model on a fast link already spends 97% of its startup fetching
bytes. With a 7B or 70B model the download dominates far more, which is why
in-cluster mirrors, image streaming and pre-staged weights matter.

## Together with buildtrace: how long, and from whom

coldstart says *how long* each phase took; [buildtrace](https://github.com/officialasishkumar/buildtrace),
run as a sidecar in the same pod, says *where those bytes came from*. One
pod, both views (kind, 24 Nov 2026, [manifest](https://github.com/officialasishkumar/buildtrace/blob/main/deploy/llm-pod-with-buildtrace.yaml)):

```
coldstart:   created → first response 51.8s; init fetch-weights 41.0s (79%)
buildtrace:  us.aws.cdn.hf.co  443/tcp  446.4 MB down  Amazon.com (AS16509)  US
             huggingface.co    443/tcp      8 KB down  Amazon.com (AS16509)  US
```

Four fifths of that cold start was one download from one US-registered
network, repeated on every restart. That is the number to bring to a
discussion about node-local caches, in-cluster mirrors or data sovereignty.

## Install

```
go install github.com/officialasishkumar/coldstart@latest
```

No dependencies beyond `kubectl` on your `PATH`; coldstart drives kubectl
rather than linking a client library, so it uses your existing kubeconfig and
auth plugins unchanged.

## Commands

```
coldstart pod NAME [flags]            timeline of one existing pod
coldstart pods [-l SELECTOR] [flags]  table of many pods, with medians
coldstart run -f POD.yaml [flags]     create a pod, wait for it, measure it
coldstart file POD.json EVENTS.json   timeline from saved kubectl output
```

* `pod` works on any pod still in the cluster whose events have not expired
  (the API server keeps events for an hour by default).
* `pods -l app=vllm` prints a Markdown table, one row per pod, with medians:
  paste it straight into an issue.
* `run` creates the pod from a manifest (use `metadata.generateName` to run it
  repeatedly), waits for Ready, then optionally polls a port through the API
  server's pod proxy until a real request succeeds. Readiness probes often pass
  before a model server can answer, so the first response is the honest end of
  a cold start. `--delete` cleans up.
* `file` rebuilds a timeline from `kubectl get pod -o json` and `kubectl get
  events -o json` output, so someone can send you their numbers.

Common flags: `--context`, `--kubeconfig`, `-n`, `--gpu-hour PRICE`,
`--currency SYMBOL`, `--gpus N` (default: read `*/gpu` resources from the pod),
`--json FILE`.

## How it decides

| phase | from | to |
|---|---|---|
| schedule | pod created | `PodScheduled` condition (notes `FailedScheduling` if it waited for capacity) |
| image pull | `Pulling` event | that time plus the "including waiting" duration from the `Pulled` event; size from `Image size: N bytes` (kubelet 1.30+) |
| init | init container `startedAt` | `finishedAt` |
| warm-up | last app container `startedAt` | `Ready` condition |
| first response | `Ready` | first successful probe (`run --probe-port` only) |
| setup | any gap between the above: sandbox, CNI, volume mounts, container create |

Overlaps are assigned to whichever phase started first, so the phases always
add up to the total. Idle accelerator time runs from scheduling to the end of
the timeline, the window in which the pod holds its GPUs without serving.
Kubernetes timestamps on conditions and older events have one-second
resolution; pull durations come from the kubelet's own measurement and are
millisecond-precise.

## Limits

* It measures the pod, not the node: time to provision a new GPU node shows up
  inside "schedule", not separately.
* Restarted containers: Kubernetes keeps only the last previous state, so a pod
  that crash-looped several times is flagged in the notes rather than broken
  down per attempt.
* Pairs with [buildtrace](https://github.com/officialasishkumar/buildtrace),
  which shows which hosts, operators and countries those downloaded bytes came
  from.

## License

MIT
