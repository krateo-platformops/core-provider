package composition

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
)

// #265. A blueprint referencing a kind the cluster does not serve and chart-inspector being broken
// used to be the same 500, folded into Synced's message. ChartRenderable names which.
func TestChartRenderableFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantReason string
		why        string
	}{
		{
			name:       "422 is the blueprint's unserved kind",
			err:        fmt.Errorf("wrapped: %w", &chartinspector.StatusError{Code: http.StatusUnprocessableEntity, Body: `{"reason":"UnprocessableEntity"}`}),
			wantReason: reasonUnservedKind,
			why:        "this is the caller's blueprint disagreeing with the cluster, and it self-resolves once the CRD exists",
		},
		{
			name:       "500 is chart-inspector's own failure",
			err:        fmt.Errorf("wrapped: %w", &chartinspector.StatusError{Code: http.StatusInternalServerError, Body: "boom"}),
			wantReason: reasonInspectorUnavailable,
			why:        "a service fault must NOT be attributed to the blueprint, or triage is no better than the 500 it replaced",
		},
		{
			name:       "503 is also chart-inspector's",
			err:        &chartinspector.StatusError{Code: http.StatusServiceUnavailable, Body: ""},
			wantReason: reasonInspectorUnavailable,
			why:        "every 5xx is the service's",
		},
		{
			name:       "a 4xx that is not 422 is not an unserved kind",
			err:        &chartinspector.StatusError{Code: http.StatusBadRequest, Body: "missing query parameters"},
			wantReason: reasonInspectorUnavailable,
			why:        "only 422 carries the unserved-kind meaning; a 400 is the cdc calling the API wrongly, which is not the blueprint author's problem to fix",
		},
		{
			name:       "no status at all",
			err:        errors.New("dial tcp: connection refused"),
			wantReason: reasonInspectorUnavailable,
			why:        "a request that never got an answer cannot implicate the blueprint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, message := chartRenderableFailure(tc.err)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q — %s", reason, tc.wantReason, tc.why)
			}
			if message == "" {
				t.Error("an empty message leaves an operator with a reason and no next action")
			}
		})
	}
}

// The UnservedKind message must say the thing an operator can act on. The whole defect was that
// the actionable sentence existed in chart-inspector's body and never reached the condition.
func TestUnservedKindMessageIsActionable(t *testing.T) {
	_, msg := chartRenderableFailure(&chartinspector.StatusError{Code: http.StatusUnprocessableEntity})
	if !strings.Contains(msg, "CRD") {
		t.Errorf("message %q does not mention the CRD; 'install the missing CRD' is the only action "+
			"available to the reader", msg)
	}
	// And it must NOT read as permanent. A 422 means "not renderable as things stand" — the
	// detector cannot distinguish a kind that will never exist from one whose CRD was created
	// moments ago, so a message implying permanence invites a consumer to latch (#261, #273).
	for _, forbidden := range []string{"never", "cannot be", "unsupported", "invalid"} {
		if strings.Contains(strings.ToLower(msg), forbidden) {
			t.Errorf("message %q contains %q, which reads as permanent; a 422 is 'not now', and a "+
				"consumer that latches on it freezes a composition on a transient window", msg, forbidden)
		}
	}
}

// Classified by errors.As on a typed status, never by matching message text. helm's wording is not
// ours to depend on and has changed under us before.
func TestClassificationDoesNotDependOnMessageText(t *testing.T) {
	// The real 422 body text, as a BARE error with no typed status in the chain. It must NOT
	// classify as an unserved kind: without the status there is no evidence of one.
	bare := errors.New(`unexpected status code: 422 - response body: {"message":"no matches for kind \"RemoteMCPServer\"","reason":"UnprocessableEntity","code":422}`)
	reason, _ := chartRenderableFailure(bare)
	if reason == reasonUnservedKind {
		t.Error("classified from message text alone. The typed status is the evidence; matching " +
			"strings would make this break the next time helm or plumbing rewords a body")
	}

	// CONTROL: the same information carried as a typed status DOES classify, so the test above is
	// about the mechanism rather than about the classifier being inert.
	typed := fmt.Errorf("x: %w", &chartinspector.StatusError{Code: http.StatusUnprocessableEntity})
	if r, _ := chartRenderableFailure(typed); r != reasonUnservedKind {
		t.Errorf("control: a typed 422 must classify as UnservedKind, got %q", r)
	}
}
