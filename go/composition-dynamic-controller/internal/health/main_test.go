package health

import (
	"os"
	"testing"

	clientfeatures "k8s.io/client-go/features"
)

// TestMain switches client-go's WatchListClient gate off, as unstructured-runtime's own controller
// tests do. The gate is on by default in client-go v0.35, and the fake dynamic client never sends
// the initial-events bookmark a watch-list stream needs, so with the gate on the informer would wait
// on the stream instead of reaching the LIST the probe test controls. The readiness decision in
// watch-list mode is covered where it is made, in unstructured-runtime
// (TestReadyzCheck_WatchListMode_NotReadyUntilInitialEventsEnd_ThenReady).
func TestMain(m *testing.M) {
	type featureSetter interface {
		Set(clientfeatures.Feature, bool) error
	}
	if fg, ok := clientfeatures.FeatureGates().(featureSetter); ok {
		if err := fg.Set(clientfeatures.WatchListClient, false); err != nil {
			panic("failed to disable WatchListClient feature gate: " + err.Error())
		}
	}
	os.Exit(m.Run())
}
