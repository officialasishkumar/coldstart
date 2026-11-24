// Package kube holds the few Kubernetes API fields coldstart reads, and a thin
// wrapper around kubectl so the tool needs no client library.
package kube

import "time"

// Pod is the subset of core/v1 Pod that matters for startup timing.
type Pod struct {
	Metadata struct {
		Name              string     `json:"name"`
		Namespace         string     `json:"namespace"`
		UID               string     `json:"uid"`
		CreationTimestamp time.Time  `json:"creationTimestamp"`
		DeletionTimestamp *time.Time `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string      `json:"nodeName"`
		InitContainers []Container `json:"initContainers"`
		Containers     []Container `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		Conditions            []Condition       `json:"conditions"`
		InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []ContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// Container is a container spec: name, image and resources.
type Container struct {
	Name          string `json:"name"`
	Image         string `json:"image"`
	RestartPolicy string `json:"restartPolicy"` // "Always" on an init container makes it a sidecar
	Resources     struct {
		Limits   map[string]string `json:"limits"`
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
}

// Condition is a pod condition such as PodScheduled or Ready.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// ContainerStatus is the kubelet's view of one container.
type ContainerStatus struct {
	Name         string         `json:"name"`
	Image        string         `json:"image"`
	Ready        bool           `json:"ready"`
	Started      *bool          `json:"started"`
	RestartCount int            `json:"restartCount"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState"`
}

// ContainerState holds exactly one of running, terminated or waiting.
type ContainerState struct {
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		StartedAt  time.Time `json:"startedAt"`
		FinishedAt time.Time `json:"finishedAt"`
		ExitCode   int       `json:"exitCode"`
		Reason     string    `json:"reason"`
	} `json:"terminated"`
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
}

// Event is the subset of core/v1 Event coldstart uses.
type Event struct {
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		UID       string `json:"uid"`
		FieldPath string `json:"fieldPath"`
	} `json:"involvedObject"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Type           string     `json:"type"`
	Count          int        `json:"count"`
	FirstTimestamp *time.Time `json:"firstTimestamp"`
	LastTimestamp  *time.Time `json:"lastTimestamp"`
	EventTime      *time.Time `json:"eventTime"`
}

// When returns the best timestamp the event carries: eventTime has
// microseconds, firstTimestamp only seconds.
func (e Event) When() time.Time {
	switch {
	case e.EventTime != nil && !e.EventTime.IsZero():
		return *e.EventTime
	case e.FirstTimestamp != nil && !e.FirstTimestamp.IsZero():
		return *e.FirstTimestamp
	case e.LastTimestamp != nil:
		return *e.LastTimestamp
	}
	return time.Time{}
}

// PodList and EventList wrap kubectl's list output.
type PodList struct {
	Items []Pod `json:"items"`
}

type EventList struct {
	Items []Event `json:"items"`
}

// Condition returns the named condition's transition time if it is True.
func (p *Pod) Condition(t string) (time.Time, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == t && c.Status == "True" {
			return c.LastTransitionTime, true
		}
	}
	return time.Time{}, false
}
