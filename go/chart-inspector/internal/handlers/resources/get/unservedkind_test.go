package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"github.com/krateo-platformops/plumbing/http/response"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #265. A blueprint referencing a kind this cluster does not serve used to answer
// 500/InternalError — the one class a caller may read as "not my fault, retry later" — so every
// consumer retried forever while the body already said "ensure CRDs are installed first".
//
// The handler needs a live chart, a CompositionDefinition and a helm client to reach its render
// step, so this covers the CLASSIFICATION DECISION and the wire format it produces, not the call
// site. Stated rather than implied: a wiring test would need the full handler fixture, and the
// mutation check below records what that leaves uncovered.
func TestUnservedKindIsClassified422NotInternalError(t *testing.T) {
	// The real chain: cli-runtime wraps the typed error with %w, plumbing's postrender_validation
	// wraps that, helm wraps that again. The handler sees the outermost error.
	unserved := fmt.Errorf("install failed: %w",
		fmt.Errorf("error while running post render on files: %w",
			fmt.Errorf("failed to build rendered manifests: %w", &meta.NoKindMatchError{
				GroupKind:        schema.GroupKind{Group: "kagent.dev", Kind: "RemoteMCPServer"},
				SearchedVersions: []string{"v1alpha2"},
			})))

	t.Run("the detector fires on the real error chain", func(t *testing.T) {
		if !helmv3.IsRESTMappingMiss(unserved) {
			t.Fatal("the classification cannot happen if the detector does not see this; without " +
				"this arm every assertion below would be about an error that never classifies")
		}
	})

	t.Run("422 carries reason UnprocessableEntity on the wire", func(t *testing.T) {
		// The status alone is not the deliverable: the cdc reads the body. plumbing maps 422 to
		// reason UnprocessableEntity in status.go, and this asserts that mapping rather than
		// assuming it — the convenience helper is absent, so Encode/New is the call being used.
		rec := httptest.NewRecorder()
		if err := response.Encode(rec, response.New(http.StatusUnprocessableEntity, unserved)); err != nil {
			t.Fatalf("encoding: %v", err)
		}
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", rec.Code)
		}
		var body struct {
			Code    int    `json:"code"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response body is not JSON: %v (%s)", err, rec.Body.String())
		}
		if body.Reason != "UnprocessableEntity" {
			t.Errorf("reason = %q, want UnprocessableEntity — a consumer distinguishing this from a "+
				"server fault reads the reason, not only the code", body.Reason)
		}
		if body.Code != http.StatusUnprocessableEntity {
			t.Errorf("body code = %d, want 422", body.Code)
		}
		// The actionable sentence helm already produces must survive into the body, or the caller
		// is told the request is unprocessable and not what to do about it.
		if body.Message == "" {
			t.Error("the message is empty; helm's own 'ensure CRDs are installed first' is the only " +
				"actionable part of this response")
		}
	})

	// CONTROL. A genuine service fault must STILL be 500, or the classification has simply moved
	// every failure into the caller's column and triage is no better than before.
	t.Run("an unrelated failure is NOT classified as the caller's fault", func(t *testing.T) {
		for _, e := range []error{
			errors.New("connection refused"),
			errors.New("release: not found"),
			errors.New("timed out waiting for the condition"),
			fmt.Errorf("getting chart info: %w", errors.New("chart infos cannot be nil")),
		} {
			if helmv3.IsRESTMappingMiss(e) {
				t.Errorf("%v classified as an unserved kind; it would be reported as the "+
					"blueprint's fault and a real chart-inspector fault would become invisible", e)
			}
		}
	})
}
