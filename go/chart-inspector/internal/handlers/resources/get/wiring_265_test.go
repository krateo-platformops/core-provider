package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krateo-platformops/chart-inspector/internal/handlers"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// #265 WIRING. unservedkind_test.go covers the classification DECISION and the wire format. A
// compiling mutant that evaluates the detector and never acts on it SURVIVES those — so they say
// nothing about whether the handler classifies. This drives the real handler.
//
// The assertion is on the STATUS CODE from an end-to-end request, which is the only thing a
// consumer sees.

type unservedKindHelmClient struct{ helmconfig.Client }

func (c unservedKindHelmClient) Install(context.Context, string, string, *helmconfig.InstallConfig) (*helmconfig.Release, error) {
	// The real chain, as cli-runtime / plumbing / helm wrap it.
	return nil, fmt.Errorf("install failed: %w",
		fmt.Errorf("error while running post render on files: %w",
			fmt.Errorf("failed to build rendered manifests: %w", &meta.NoKindMatchError{
				GroupKind:        schema.GroupKind{Group: "kagent.dev", Kind: "RemoteMCPServer"},
				SearchedVersions: []string{"v1alpha2"},
			})))
}

type brokenHelmClient struct{ helmconfig.Client }

func (c brokenHelmClient) Install(context.Context, string, string, *helmconfig.InstallConfig) (*helmconfig.Release, error) {
	return nil, fmt.Errorf("dialing the registry: connection refused")
}

type stubPluralizer struct{}

func (stubPluralizer) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: "fireworks"}, nil
}

func wiringFixture(t *testing.T, hc helmconfig.Client) http.Handler {
	t.Helper()
	cd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
		"metadata": map[string]any{"name": "fireworksapp", "namespace": "krateo-system"},
		"spec": map[string]any{"chart": map[string]any{
			"url": "oci://ghcr.io/example/charts/fireworks", "version": "1.0.0",
		}},
	}}
	comp := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1-0-0", "kind": "Firework",
		"metadata": map[string]any{"name": "fw", "namespace": "krateo-system"},
		"spec":     map[string]any{},
	}}

	scheme := runtime.NewScheme()
	gvrToKind := map[schema.GroupVersionResource]string{
		{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}: "CompositionDefinitionList",
		{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworks"}:         "FireworksList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToKind, cd, comp)

	return GetResources(handlers.HandlerOptions{
		Log:             slog.Default(),
		DynamicClient:   dyn,
		KrateoNamespace: "krateo-system",
		Plurarizer:      stubPluralizer{},
		RestConfig:      &rest.Config{Host: "https://api.example.invalid:6443"},
		HelmClient:      hc,
	})
}

func doRequest(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/resources?"+
		"compositionName=fw&compositionNamespace=krateo-system"+
		"&compositionDefinitionName=fireworksapp&compositionDefinitionNamespace=krateo-system"+
		"&compositionVersion=v1-0-0&compositionResource=fireworks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// requireReachedRender fails the test unless the response came FROM the render step. Without it
// both tests below pass on a fixture that never gets that far: a composition the fake client
// cannot find also produces 500, which is exactly what the control asserts — so the control was
// green while exercising nothing. The discriminator is the body: helm's own wrapper text.
func requireReachedRender(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	if !strings.Contains(body, "install failed") && !strings.Contains(body, "dialing the registry") {
		t.Fatalf("VACUOUS: the response did not come from the render step, so this test asserts "+
			"nothing about the classification. status=%d body=%s", rec.Code, body)
	}
}

func TestHandler_UnservedKindAnswers422NotInternalError(t *testing.T) {
	rec := doRequest(t, wiringFixture(t, unservedKindHelmClient{}))

	requireReachedRender(t, rec)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("a chart referencing an unserved kind answered 500/InternalError. That is the one "+
			"class a caller may read as 'not my fault, retry later', so every consumer retries "+
			"forever while the body already says 'ensure CRDs are installed first' (#265). body: %s",
			rec.Body.String())
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
	}

	var body struct{ Reason string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Reason != "UnprocessableEntity" {
		t.Errorf("reason = %q, want UnprocessableEntity", body.Reason)
	}
}

// CONTROL. A genuine service fault must STILL be 500 through the same handler, or the
// classification has moved every failure into the caller's column and triage is no better.
func TestHandler_UnrelatedFailureStillAnswers500(t *testing.T) {
	rec := doRequest(t, wiringFixture(t, brokenHelmClient{}))

	requireReachedRender(t, rec)
	if rec.Code == http.StatusUnprocessableEntity {
		t.Fatalf("a connection-refused answered 422; a real chart-inspector fault is now reported "+
			"as the blueprint's problem. body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for an unrelated failure. body: %s", rec.Code, rec.Body.String())
	}
}
