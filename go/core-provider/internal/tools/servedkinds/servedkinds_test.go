package servedkinds

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// mapperServing is a RESTMapper that serves exactly the given Kinds, as the API server's discovery
// would after their CRDs are installed.
func mapperServing(kinds ...schema.GroupVersionKind) meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range kinds {
		m.Add(gvk, meta.RESTScopeNamespace)
	}
	return m
}

// erroringMapper fails every lookup with err, as a mapper does when discovery cannot reach the API
// server.
type erroringMapper struct {
	meta.RESTMapper
	err error
}

func (e erroringMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, e.err
}

func TestCheck(t *testing.T) {
	cd, kt := Required[0], Required[1]

	t.Run("every Kind served: no error (the positive control)", func(t *testing.T) {
		if err := Check(mapperServing(cd, kt), Required, "2.13.114"); err != nil {
			t.Fatalf("Check = %v, want nil", err)
		}
	})

	t.Run("no CRD installed: names both, the chart and the version", func(t *testing.T) {
		err := Check(mapperServing(), Required, "2.13.114")
		var ns *NotServedError
		if !errors.As(err, &ns) {
			t.Fatalf("Check = %v (%T), want *NotServedError", err, err)
		}
		if len(ns.Missing) != 2 {
			t.Errorf("Missing = %v, want both Required Kinds", ns.Missing)
		}
		msg := err.Error()
		for _, want := range []string{
			"CompositionDefinition.core.krateo.io/v1alpha1",
			"KubernetesTarget.core.krateo.io/v1alpha1",
			"install core-provider-crds at version 2.13.114",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q does not contain %q", msg, want)
			}
		}
	})

	t.Run("one CRD missing: names only that one", func(t *testing.T) {
		err := Check(mapperServing(cd), Required, "2.13.114")
		var ns *NotServedError
		if !errors.As(err, &ns) {
			t.Fatalf("Check = %v, want *NotServedError", err)
		}
		if len(ns.Missing) != 1 || ns.Missing[0] != kt {
			t.Errorf("Missing = %v, want only %v", ns.Missing, kt)
		}
		if strings.Contains(err.Error(), "CompositionDefinition.") {
			t.Errorf("message names the served CompositionDefinition: %q", err)
		}
	})

	t.Run("the Kind served at another version only: not served", func(t *testing.T) {
		other := schema.GroupVersionKind{Group: cd.Group, Version: "v1alpha2", Kind: cd.Kind}
		err := Check(mapperServing(other, kt), Required, "2.13.114")
		if !IsNotServed(err) {
			t.Fatalf("Check = %v, want not served: the binary reads %s", err, cd.Version)
		}
	})

	t.Run("discovery failure: a failure to check, never missing CRDs", func(t *testing.T) {
		boom := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
		err := Check(erroringMapper{err: boom}, Required, "2.13.114")
		if err == nil {
			t.Fatal("Check = nil, want an error")
		}
		if IsNotServed(err) {
			t.Fatalf("Check = %v: an unreachable API server was reported as missing CRDs", err)
		}
		if !errors.Is(err, boom) {
			t.Errorf("Check = %v, want it to wrap the discovery error", err)
		}
		if strings.Contains(err.Error(), CRDsChart) {
			t.Errorf("a failure to check tells the reader to install %s: %q", CRDsChart, err)
		}
	})
}
