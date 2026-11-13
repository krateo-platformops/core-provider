package resources

import (
	"context"
	"encoding/json"
	"errors"
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

func (c unservedKindHelmClient) Install(context.Context, string, string, *helmconfig.InstallConfig) (*helmconfig.OperationResult, error) {
	// The real chain, as cli-runtime / plumbing / helm wrap it.
	return nil, fmt.Errorf("install failed: %w",
		fmt.Errorf("error while running post render on files: %w",
			fmt.Errorf("failed to build rendered manifests: %w", &meta.NoKindMatchError{
				GroupKind:        schema.GroupKind{Group: "kagent.dev", Kind: "RemoteMCPServer"},
				SearchedVersions: []string{"v1alpha2"},
			})))
}

type brokenHelmClient struct{ helmconfig.Client }

func (c brokenHelmClient) Install(context.Context, string, string, *helmconfig.InstallConfig) (*helmconfig.OperationResult, error) {
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
		"&compositionVersion=v1-0-0&compositionResource=fireworks"+
		// releaseName is REQUIRED since #329. Without it the handler answers 400 before reaching
		// the render step, and this test's own vacuity guard says so rather than passing.
		"&releaseName=fw-abcdefgh", nil)
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

// recordingHelmClient captures the release name the handler passes to Install.
type recordingHelmClient struct {
	helmconfig.Client
	got *string
}

func (c recordingHelmClient) Install(_ context.Context, name string, _ string, _ *helmconfig.InstallConfig) (*helmconfig.OperationResult, error) {
	*c.got = name
	return nil, errors.New("install failed: recorded and stopped")
}

// #329. THE NAME THE SERVER USES MUST BE THE CALLER'S, not the composition's label.
//
// This exists because a mutant survived: replacing `releaseName` with `""` at the Install call
// broke nothing. The 400-when-absent test does not cover it — a server that accepts the parameter
// and then ignores it passes that test and reproduces the original defect exactly.
//
// The fixture's composition deliberately carries NO krateo.io/release-name label, which is the
// adoption path's state: if the handler ever reads the label again, it gets "" and this fails,
// which is the failure the openstack-ironic-operator-kog session measured as a permanent retry
// loop on a live cluster.
func TestHandler_UsesTheCallersReleaseNameNotTheCompositionLabel(t *testing.T) {
	var got string
	rec := doRequest(t, wiringFixture(t, recordingHelmClient{got: &got}))
	requireReachedRender(t, rec)

	if got != "fw-abcdefgh" {
		t.Errorf("Install was called with release name %q, want %q (the releaseName query "+
			"parameter). The fixture's composition has NO krateo.io/release-name label, so %q "+
			"means the handler went back to reading the label -- which on an adoption path is "+
			"empty, makes helm answer `no name provided`, and wedges the caller in a retry loop "+
			"it cannot escape (core-provider#329)", got, "fw-abcdefgh", got)
	}
}

// #329. An absent releaseName must be a 400 that NAMES the parameter.
//
// Lives here rather than beside the other required-parameter cases because those are in
// resources_test.go, which is behind `//go:build integration` and does not run in the default
// suite — so the mutant that deletes this guard SURVIVED until this test existed. A case in a
// file the default build excludes is coverage of the integration job, not of the package.
//
// 400 AND NOT 500, and the distinction is the whole defect. A 500 is the one class a caller is
// entitled to read as "not my fault, retry later", so core-provider retried at 1s, 2s, 4s, 8s,
// 16s forever — and no amount of retrying can supply a parameter the caller never sent. The
// parameter name is in the body because the one sentence that resolves this must not require
// reading this source.
func TestHandler_MissingReleaseNameIs400AndNamesTheParameter(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/resources?"+
		"compositionName=fw&compositionNamespace=krateo-system"+
		"&compositionDefinitionName=fireworksapp&compositionDefinitionNamespace=krateo-system"+
		"&compositionVersion=v1-0-0&compositionResource=fireworks", nil)
	rec := httptest.NewRecorder()
	wiringFixture(t, unservedKindHelmClient{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Every other required parameter is present, so this can "+
			"only be about releaseName -- and a 500 here is what sent the caller into a permanent "+
			"retry loop. body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "releaseName") {
		t.Errorf("the 400 body does not name the parameter: %s", rec.Body.String())
	}

	// CONTROL: the same fixture WITH the parameter must get past this guard and reach the render
	// step. Without it, a handler that rejected every request would satisfy the assertions above.
	requireReachedRender(t, doRequest(t, wiringFixture(t, unservedKindHelmClient{})))
}
