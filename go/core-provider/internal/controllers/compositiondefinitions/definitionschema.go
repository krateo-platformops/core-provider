package compositiondefinitions

import (
	"context"
	"fmt"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EVERY DEFINITION PUBLISHES ITS SCHEMA IN ITS OWN NAMESPACE (Diego, 2026-11-16: "every CD's
// namespace"). #542 moved the ConfigMap Deploy writes into the definition's namespace, but only an
// OWNER runs Deploy. A follower, a definition whose Kind+version's controller another definition
// owns, published nothing in its own namespace, so a tenant reading it found no schema and the portal
// drew no form. A follower now writes its own copy (Update), reports it missing or stale (Observe),
// and a definition handing its controller over on delete removes its copy unless another definition
// in the same namespace publishes the same object.

// publishOwnSchema writes cr's own copy of gvr's schema, on the hub.
func (e *external) publishOwnSchema(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource, specSchemaBytes []byte) error {
	return deploy.ApplyDefinitionSchemaConfigMap(ctx, e.mgmtDynamic, gvr, cr.Namespace, JSONSchemaTemplateConfigmapPath, specSchemaBytes)
}

// ownSchemaCurrent reports whether cr's own copy exists with exactly this schema, and if not, why.
func (e *external) ownSchemaCurrent(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource, specSchemaBytes []byte) (bool, string, error) {
	return deploy.DefinitionSchemaConfigMapCurrent(ctx, e.mgmtDynamic, gvr, cr.Namespace, JSONSchemaTemplateConfigmapPath, specSchemaBytes)
}

// schemaSharer returns another live definition in cr's namespace that targets gvk, by status or by
// spec (targetsVersion), or nil. Such a definition publishes the SAME ConfigMap, same namespace and
// same derived name, so it is that one's too.
func schemaSharer(cr *compositiondefinitionsv1alpha1.CompositionDefinition, cds []compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, chart chartRef) *compositiondefinitionsv1alpha1.CompositionDefinition {
	for i := range cds {
		cd := &cds[i]
		if cd.Namespace != cr.Namespace || cd.Name == cr.Name || cd.GetDeletionTimestamp() != nil {
			continue
		}
		if targetsVersion(cd, gvk, chart) {
			return cd
		}
	}
	return nil
}

// ownSchemaSharer is schemaSharer over the live definitions in cr's namespace. A failed list is an
// error: "could not determine" is never "no one else uses it".
func (e *external) ownSchemaSharer(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind) (*compositiondefinitionsv1alpha1.CompositionDefinition, error) {
	var all compositiondefinitionsv1alpha1.CompositionDefinitionList
	if err := e.mgmt.List(ctx, &all, client.InNamespace(cr.Namespace)); err != nil {
		return nil, fmt.Errorf("error listing CompositionDefinitions in %s: %w", cr.Namespace, err)
	}
	return schemaSharer(cr, all.Items, gvk, chartRefOf(cr.Spec.Chart)), nil
}

// removeOwnSchema removes cr's own copy on a delete that hands the controller over, a path that
// returns before Undeploy and so used to leave the copy behind. A failed list removes nothing and
// is returned, so the delete retries.
func (e *external) removeOwnSchema(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource, log logging.Logger) error {
	sharer, err := e.ownSchemaSharer(ctx, cr, gvk)
	if err != nil {
		return fmt.Errorf("before removing the JSON schema ConfigMap: %w", err)
	}
	if sharer != nil {
		log.Debug("Keeping the JSON schema ConfigMap: another definition in the namespace publishes it",
			"gvr", gvr.String(), "namespace", cr.Namespace, "by", sharer.Name)
		return nil
	}
	return deploy.RemoveDefinitionSchemaConfigMap(ctx, e.mgmtDynamic, gvr, cr.Namespace)
}

// ownSchemaRemains reports whether cr's own copy still exists with nobody else in the namespace
// publishing it, i.e. whether removeOwnSchema still has something to do. handedOver asks it, so a
// definition being deleted is not reported gone (and Delete skipped) while its copy is left behind:
// the rehome integration test caught exactly that, the finalizer dropping with the copy in place.
// It is the same predicate removeOwnSchema acts on, so once Delete has run it answers false.
func (e *external) ownSchemaRemains(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) (bool, error) {
	exists, err := deploy.DefinitionSchemaConfigMapExists(ctx, e.mgmtDynamic, gvr, cr.Namespace)
	if err != nil || !exists {
		return exists, err
	}
	sharer, err := e.ownSchemaSharer(ctx, cr, gvk)
	if err != nil {
		return false, err
	}
	return sharer == nil, nil
}
