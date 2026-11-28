package composition

import (
	"context"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/snowplow"
	"github.com/krateo-platformops/plumbing/apiref"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SnowplowAPIResolver resolves a single apiRef RESTAction (via snowplow, under the CDC's
// authn identity) into the keyed ".api" projection source. It satisfies APIResolver.
//
// Extras are layered: the author-declared static extras (CompositionDefinition
// apiRef.extras) form the base, and per-instance context computed from the composition (name,
// namespace, id) merges over them — request-wins — before snowplow further merges the result
// over the RESTAction's own inline extras.
type SnowplowAPIResolver struct {
	client *snowplow.Client
	ref    snowplow.ApiRef
	extras map[string]any
}

// NewSnowplowAPIResolver builds a resolver for the given RESTAction reference and static
// extras. extras may be nil.
func NewSnowplowAPIResolver(client *snowplow.Client, ref snowplow.ApiRef, extras map[string]any) *SnowplowAPIResolver {
	return &SnowplowAPIResolver{client: client, ref: ref, extras: extras}
}

func (r *SnowplowAPIResolver) Resolve(ctx context.Context, mg *unstructured.Unstructured) (snowplow.Resolution, error) {
	return r.client.Resolve(ctx, r.ref, r.mergedExtras(mg))
}

// mergedExtras layers per-instance composition context over the static author extras.
func (r *SnowplowAPIResolver) mergedExtras(mg *unstructured.Unstructured) map[string]any {
	// One builder, shared with core-provider's /rbac request (core-provider#691): the two must agree.
	return apiref.CompositionExtras(r.extras, mg.GetName(), mg.GetNamespace(), string(mg.GetUID()))
}
