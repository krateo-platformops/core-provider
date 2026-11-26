// Package writtenby is the cdc's half of core-provider#669: the stamp it puts on every composition
// status it writes, so a reader of the object (who may not be allowed to read the controller's
// Deployment) knows which release's reason table describes its conditions.
//
// core-provider has the other half, for CompositionDefinitions, in its own module.
package writtenby

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Name is status.writtenBy.name on a composition. docs/ready-reasons.json is the table for it, and
// its notice says so (TestWrittenByNamesMapToOneTableEach, internal/composition).
const Name = "composition-dynamic-controller"

// Stamp is this binary's build stamp, as the build carries it: a release build's bare tag
// (2.13.135) and commit, or "dev"/"unknown" on a build that was not stamped.
type Stamp struct {
	Version string
	Commit  string
}

// IsZero reports a stamp nobody set (a handler built without one, as in tests).
func (s Stamp) IsZero() bool { return s == Stamp{} }

// Set puts the stamp on mg's in-memory status as status.writtenBy. It never writes: it is called
// where a writer is about to send the status anyway, so a pass that changes nothing else sends the
// stored value back and the apiserver records nothing. A zero stamp leaves the field as it is.
func (s Stamp) Set(mg *unstructured.Unstructured) error {
	if mg == nil || s.IsZero() {
		return nil
	}
	return unstructured.SetNestedStringMap(mg.Object, map[string]string{
		"name":    Name,
		"version": s.Version,
		"commit":  s.Commit,
	}, "status", "writtenBy")
}

type contextKey struct{}

// NewContext carries s to every status write made under ctx. The handler puts it on each entry
// point's context, so the write funnel (updateStatus) stamps every status the handler sends without
// each helper having to carry it.
func NewContext(ctx context.Context, s Stamp) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// FromContext is the stamp NewContext put on ctx, or a zero one.
func FromContext(ctx context.Context) Stamp {
	s, _ := ctx.Value(contextKey{}).(Stamp)
	return s
}
