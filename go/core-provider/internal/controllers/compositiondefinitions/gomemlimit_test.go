package compositiondefinitions

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// #202. A CDC's RSS tracks the arena HIGH-WATER MARK, not its live data: measured on krateo-057,
// agentgatewaycontrollers held HeapSys 206.5 MiB against HeapAlloc of 20.7 MiB, having allocated
// 1,548 GB in 15 hours across 58,149 collections. Every resync renders the chart and runs a helm
// 3-way merge (~8s), Go grows the arena to that burst peak, and returns it only lazily.
//
// GOMEMLIMIT caps it. These assert the SHAPE of the declaration, because every way of getting it
// subtly wrong fails silently — Go ignores a GOMEMLIMIT it cannot parse and simply runs unbounded,
// so a typo here is indistinguishable from not having the feature.
func TestCDCDeployment_DeclaresGoMemLimit(t *testing.T) {
	dep := renderCDCDeployment(t, "")

	var env *corev1.EnvVar
	for i, c := range dep.Spec.Template.Spec.Containers {
		for j, e := range c.Env {
			if e.Name == "GOMEMLIMIT" {
				env = &dep.Spec.Template.Spec.Containers[i].Env[j]
			}
		}
	}
	if env == nil {
		t.Fatal("no GOMEMLIMIT on the rendered CDC container; the runtime then grows its arena to " +
			"the render burst peak and keeps it, which is #202")
	}

	// Derived, never hardcoded. A literal would drift the moment anyone changed the request --
	// including per-Kind, via spec.controller.resources, which the operator can set per definition.
	if env.ValueFrom == nil || env.ValueFrom.ResourceFieldRef == nil {
		t.Fatalf("GOMEMLIMIT is not derived from a resource field (value=%q); a hardcoded limit "+
			"silently stops matching the container the moment its resources change", env.Value)
	}
	ref := env.ValueFrom.ResourceFieldRef

	if ref.Resource != "requests.memory" {
		t.Errorf("GOMEMLIMIT tracks %q, want requests.memory: the point is that the pod lives "+
			"inside what it RESERVED, which is the contract the scheduler was given", ref.Resource)
	}

	// The container must name ITSELF. Pointing at another container, or leaving it empty, yields a
	// number unrelated to this process -- and an empty containerName is the easy mistake because
	// it is valid YAML and only fails at admission for a non-pod-level resource.
	wantContainer := dep.Spec.Template.Spec.Containers[0].Name
	if ref.ContainerName != wantContainer {
		t.Errorf("GOMEMLIMIT reads containerName %q, want its own container %q",
			ref.ContainerName, wantContainer)
	}

	// Bytes. GOMEMLIMIT's own syntax is "128MiB"; a Kubernetes quantity ("128Mi") does NOT parse,
	// and Go ignores an unparseable value rather than failing, so this mistake is invisible at
	// runtime. divisor 1 sidesteps it entirely by handing over a bare integer.
	if d := ref.Divisor.String(); d != "1" {
		t.Errorf("GOMEMLIMIT divisor is %q, want \"1\" so the value is bytes; any other divisor "+
			"produces a number Go will misread or ignore", d)
	}
}

// The control arm: the variable must not be set to a literal by accident, and must be the only
// env var the template introduces -- so a future addition here is a deliberate act.
func TestCDCDeployment_GoMemLimitIsTheOnlyEnvVar(t *testing.T) {
	dep := renderCDCDeployment(t, "")
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name != "GOMEMLIMIT" {
				t.Errorf("unexpected env var %q on the CDC container. The CDC takes its configuration "+
					"from the mounted ConfigMap (envFrom) and its args; an env var added here bypasses "+
					"both and will not appear in spec.controller.", e.Name)
			}
			if e.Name == "GOMEMLIMIT" && e.Value != "" {
				t.Errorf("GOMEMLIMIT is a literal %q rather than a resource reference", e.Value)
			}
		}
	}
}
