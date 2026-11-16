package deploy

import (
	"context"
	"fmt"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// THE JSON-SCHEMA CONFIGMAP IS THE COMPOSITIONDEFINITION'S PUBLISHED SCHEMA, not controller internals
// (Diego, 2026-11-16). The portal reads `<resource>-<version>-jsonschema-configmap` from the CD's
// OWN namespace, as the caller, to draw the create and edit forms. It used to be written into the
// CONTROLLER namespace (krateo-system for a local CD), which a tenant cannot read, so a tenant saw
// "This blueprint is still being registered" on a Ready CD (acmp, as tenant bob on demo). Nothing
// else reads it: the cdc never does, and core-provider only writes, hashes and removes it.
//
// It is written with the HUB client: the CD and the portal live on the management cluster, so for
// a remote definition, a ConfigMap written with the target cluster's client was never where anyone
// looked for it.

// hubOr returns hub when set, else dyn (a local definition: one cluster).
func hubOr(hub, dyn dynamic.Interface) dynamic.Interface {
	if hub != nil {
		return hub
	}
	return dyn
}

// schemaConfigMapNN is where the CD's schema ConfigMap lives: in the CD's own namespace, under the
// name derived from the controller object name (`<resource>-<version>`), exactly as before.
func schemaConfigMapNN(cdNamespace string, controllerObject types.NamespacedName) types.NamespacedName {
	return getJsonSchemaConfigmapNN(types.NamespacedName{Namespace: cdNamespace, Name: controllerObject.Name})
}

// legacySchemaConfigMapRemovable reports whether the copy a previous core-provider wrote into the
// CONTROLLER namespace may be removed, and why. Every arm but the last keeps it, because removing the
// wrong one empties a form, and "could not determine" is never "absent":
//
//   - the controller namespace IS the CD's namespace: the legacy location is the CURRENT object, the
//     one just written (every CD on krateo-057 lives in krateo-system; frontend's edge);
//   - a CompositionDefinition of the same resource lives in the controller namespace: that copy is
//     ITS published schema, not a leftover (a leader there with a follower elsewhere);
//   - the CD list failed: nothing is known, so nothing is removed.
//
// It never matches by suffix: the caller removes exactly one object by its derived name, so the
// chart's own template ConfigMap (`<release>-jsonschema-configmap`) is never a candidate.
func legacySchemaConfigMapRemovable(ctx context.Context, hub dynamic.Interface, controllerNamespace, cdNamespace string, gvr schema.GroupVersionResource) (bool, string) {
	if controllerNamespace == cdNamespace {
		return false, "the controller namespace is the definition's namespace: the legacy location is the current ConfigMap"
	}
	list, err := hub.Resource(compositionDefinitionGVR).Namespace(controllerNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Sprintf("could not list CompositionDefinitions in %s: %v", controllerNamespace, err)
	}
	for _, cd := range list.Items {
		if res, _, _ := unstructured.NestedString(cd.Object, "status", "resource"); res == gvr.Resource {
			return false, fmt.Sprintf("CompositionDefinition %s/%s in the controller namespace publishes that schema", cd.GetNamespace(), cd.GetName())
		}
	}
	return true, "no CompositionDefinition of this resource lives in the controller namespace"
}

// removeLegacySchemaConfigMap deletes exactly the ConfigMap at nn (by name), when removable. A
// ConfigMap already gone is success.
func removeLegacySchemaConfigMap(ctx context.Context, hub dynamic.Interface, nn types.NamespacedName) error {
	err := hub.Resource(configMapGVR).Namespace(nn.Namespace).Delete(ctx, nn.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing the legacy schema ConfigMap %s: %w", nn, err)
	}
	return nil
}

// applySchemaConfigMap writes the CD's schema ConfigMap in the CD's namespace on the hub, then removes
// the legacy controller-namespace copy when legacySchemaConfigMapRemovable says it is a leftover, and
// never on a dry run (Deploy's drift check runs as a server dry run). It returns the ConfigMap written,
// which Deploy hashes.
func applySchemaConfigMap(ctx context.Context, opts DeployOptions, controllerObject types.NamespacedName, applyOpts kubecli.ApplyOptions, log logging.Logger) (corev1.ConfigMap, error) {
	if opts.Namespace == "" {
		return corev1.ConfigMap{}, fmt.Errorf("deploying the JSON schema ConfigMap: the CompositionDefinition's namespace is empty")
	}
	schemaNN := schemaConfigMapNN(opts.Namespace, controllerObject)
	hub := hubOr(opts.HubDynClient, opts.DynClient)
	cm, err := renderJSONSchemaConfigMap(opts.GVR, schemaNN, opts.JsonSchemaTemplatePath, opts.JsonSchemaBytes)
	if err != nil {
		return corev1.ConfigMap{}, err
	}
	if _, err := kubecli.Apply(ctx, hub, configMapGVR, &cm, applyOpts); err != nil {
		return corev1.ConfigMap{}, fmt.Errorf("error applying ConfigMap for JSON schema: %w", err)
	}
	if legacy := getJsonSchemaConfigmapNN(controllerObject); !opts.DryRunServer && legacy != schemaNN {
		if ok, why := legacySchemaConfigMapRemovable(ctx, hub, opts.ControllerNamespace, opts.Namespace, opts.GVR); ok {
			if err := removeLegacySchemaConfigMap(ctx, hub, legacy); err != nil {
				return corev1.ConfigMap{}, err
			}
			log.Info("Removed the legacy JSON schema ConfigMap from the controller namespace", "name", legacy.Name, "namespace", legacy.Namespace, "now", schemaNN.Namespace)
		} else {
			log.Debug("Keeping the JSON schema ConfigMap in the controller namespace", "name", legacy.Name, "namespace", legacy.Namespace, "reason", why)
		}
	}
	return cm, nil
}
