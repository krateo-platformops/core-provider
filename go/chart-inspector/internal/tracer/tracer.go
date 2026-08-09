package tracer

import (
	"net/http"
	"strings"
	"sync"

	"github.com/krateo-platformops/chart-inspector/internal/handlers/resources"
)

// Tracer implements http.RoundTripper.  It prints each request and
// response/error to t.OutFile.  WARNING: this may output sensitive information
// including bearer tokens.
type Tracer struct {
	http.RoundTripper
	mu        sync.Mutex
	resources []resources.Resource
}

func (t *Tracer) GetResources() []resources.Resource {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Return a copy to prevent external modification
	resCopy := make([]resources.Resource, len(t.resources))
	copy(resCopy, t.resources)
	return resCopy
}

func (t *Tracer) WithRoundTripper(rt http.RoundTripper) *Tracer {
	t.RoundTripper = rt
	return t
}

// parsePath parses a Kubernetes API request path into a Resource. It recognizes both named-object
// requests AND collection requests (no trailing name — what a Helm `lookup` of a set issues), which
// the previous length-keyed parser dropped or mis-parsed. It keys on segment ROLE — "api" vs "apis",
// the "namespaces" marker, and whether a trailing name is present — not on path length, so equal-length
// shapes (a cluster-scoped named object vs a namespaced collection) no longer collide.
//
// Verbs reflect what the observation can prove. A collection read (Name=="") can only come from a
// `lookup` of a set — a chart never creates a collection — so it is read-only (get/list/watch). A
// named observation is ambiguous (a create existence-check vs a named lookup) under server-side
// dry-run, where every request is a GET, so it conservatively stays the management superset ["*"].
func parsePath(path string) (*resources.Resource, bool) {
	split := strings.Split(strings.Trim(path, "/"), "/")
	if len(split) < 3 {
		return nil, false
	}

	var group, version string
	var tail []string
	switch split[0] {
	case "api":
		version, tail = split[1], split[2:]
	case "apis":
		if len(split) < 4 {
			return nil, false
		}
		group, version, tail = split[1], split[2], split[3:]
	default:
		return nil, false
	}

	var namespace string
	if len(tail) >= 3 && tail[0] == "namespaces" {
		// Namespace-scoped: /namespaces/{ns}/{resource}[/{name}...]. Note the guard is len>=3:
		// with only /namespaces/{x}, "namespaces" is itself the resource and {x} its name (a
		// named-namespace GET), and bare /namespaces is the namespaces collection.
		namespace, tail = tail[1], tail[2:]
	}
	if len(tail) == 0 {
		return nil, false // discovery root — nothing to grant
	}

	resource := tail[0]
	name := ""
	if len(tail) >= 2 {
		name = tail[1] // ignore tail[2:] (subresource) — the grant is on the parent resource
	}

	verbs := []string{"*"}
	if name == "" {
		verbs = []string{"get", "list", "watch"}
	}

	return &resources.Resource{
		Group:     group,
		Version:   version,
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
		Verbs:     verbs,
	}, true
}

// RoundTrip calls the nested RoundTripper while printing each request and
// response/error to t.OutFile on either side of the nested call.  WARNING: this
// may output sensitive information including bearer tokens.
func (t *Tracer) RoundTrip(req *http.Request) (*http.Response, error) {
	// Capture resource metadata under mutex protection.
	if resource, ok := parsePath(req.URL.Path); ok {
		t.mu.Lock()
		t.resources = append(t.resources, *resource)
		t.mu.Unlock()
	}

	// Call the nested RoundTripper.
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	return resp, err
}
