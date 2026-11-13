package composition

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	helm "github.com/krateo-platformops/plumbing/helm/v3"
	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
)

// The INFO line is the instrument that separates the two causes of a "no matches for kind"
// render failure: a stale shared mapper ("recovered") from apiserver discovery lag
// ("missed_again"). These pin that the line is written at INFO, through the reconcile's logger,
// with the fields an operator correlates against the CRD's served versions.
func TestLogMappingRetryWritesTheOutcomeAtInfo(t *testing.T) {
	var buf bytes.Buffer
	l := logging.NewSlogLogger(slog.New(logging.NewOTelJSONHandler(slog.LevelInfo, &buf)))
	ctx := xcontext.BuildContext(context.Background(), xcontext.WithLogger(l))

	logMappingRetry(ctx, helm.MappingRetry{
		Operation: "upgrade", Group: "composition.krateo.io", Kind: "SearchPage",
		Versions: []string{"v1-8-109"}, Outcome: helm.MappingRetryMissedAgain,
	})

	out := buf.String()
	for _, want := range []string{`"upgrade"`, `"missed_again"`, `"SearchPage"`, `v1-8-109`, "REST-mapping miss"} {
		if !strings.Contains(out, want) {
			t.Errorf("log line lacks %s: %s", want, out)
		}
	}
	if !strings.Contains(strings.ToUpper(out), "INFO") {
		t.Errorf("the retry must be logged at INFO, so the default level shows it: %s", out)
	}
}
