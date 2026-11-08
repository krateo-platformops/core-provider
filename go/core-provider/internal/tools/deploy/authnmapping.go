package deploy

import (
	"context"
	"fmt"
	"path/filepath"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	"github.com/krateo-platformops/plumbing/endpoints"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The authn allowlist mapping (serviceaccount.authn.krateo.io/ServiceAccount) authorizes the
// per-composition CDC ServiceAccount to exchange its projected token for a service JWT. It is
// created only when the CompositionDefinition declares an apiRef, and it lives in the authn
// operator namespace (where authn lists mappings), referencing the CDC SA by namespace/name.
const (
	defaultAuthnNamespace       = "krateo-system"
	authnServiceAccountTemplate = "authn-serviceaccount.yaml"
	// defaultSelfAuthnGroup is the group core-provider's own allowlist mapping belongs to when
	// CORE_PROVIDER_APIREF_GROUP is unset. Nominal — /rbac runs under snowplow's own identity.
	defaultSelfAuthnGroup = "krateo:core-provider"
)

var authnServiceAccountGVK = schema.GroupVersionKind{
	Group:   "serviceaccount.authn.krateo.io",
	Version: "v1alpha1",
	Kind:    "ServiceAccount",
}

// authnServiceAccountGVR is authnServiceAccountGVK's plural resource form (confirmed against the authn
// CRD's spec.names.plural, not guessed): required by kubecli.Apply's dynamic.Interface, which — unlike
// client.Client's REST mapper — cannot infer a GVR from a GVK.
var authnServiceAccountGVR = authnServiceAccountGVK.GroupVersion().WithResource("serviceaccounts")

func authnNamespaceOrDefault(ns string) string {
	if ns == "" {
		return defaultAuthnNamespace
	}
	return ns
}

// authnMappingName is the mapping's name — also the issued service identity's username —
// unique per composition deployment within the shared authn namespace.
func authnMappingName(compositionNamespace, saName string) string {
	return fmt.Sprintf("cdc-%s-%s", compositionNamespace, saName)
}

// cdcGroup is the per-composition group the issued identity belongs to. authn maps it to the
// clientconfig certificate's O=, so standard Kubernetes RBAC bound to this group scopes what
// the resolved RESTAction may read.
func cdcGroup(saName string) string {
	return fmt.Sprintf("krateo:cdc:%s", saName)
}

// renderAuthnServiceAccountMapping builds the mapping object (unstructured, since the type is
// owned by authn) targeting the authn namespace and referencing the CDC SA.
func renderAuthnServiceAccountMapping(opts DeployOptions, saName, saNamespace string) (*unstructured.Unstructured, error) {
	authnNS := authnNamespaceOrDefault(opts.AuthnNamespace)
	name := authnMappingName(saNamespace, saName)

	obj := &unstructured.Unstructured{}
	err := objects.CreateK8sObject(obj, opts.GVR,
		types.NamespacedName{Namespace: authnNS, Name: name},
		filepath.Join(opts.RBACFolderPath, authnServiceAccountTemplate),
		"saName", saName,
		"saNamespace", saNamespace,
		"group", cdcGroup(saName),
		"displayName", fmt.Sprintf("CDC (%s/%s)", saNamespace, saName),
	)
	if err != nil {
		return nil, fmt.Errorf("rendering authn ServiceAccount mapping: %w", err)
	}
	return obj, nil
}

// deleteAuthnClientConfigSecret deletes the session credential authn stored for a cdc's identity:
// the Secret named endpoints.SecretName(<mapping name>) in the authn namespace. The mapping name IS
// the identity's username (authn login.go), and endpoints.SecretName is the one definition of the
// Secret's name, shared with authn; it must not be rebuilt here, since a near-miss deletes nothing,
// silently.
//
// ORDER (authn's): call it only AFTER the controller has drained and its mapping has been deleted. If
// the Secret went first while the mapping existed, a stray login would make authn re-create it.
// Without the mapping, such a login gets 403 NoMapping and writes nothing.
func deleteAuthnClientConfigSecret(ctx context.Context, kube client.Client, authnNamespace, saNamespace, saName string) error {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	sec := &corev1.Secret{}
	sec.SetNamespace(authnNamespaceOrDefault(authnNamespace))
	sec.SetName(endpoints.SecretName(authnMappingName(saNamespace, saName)))
	if err := kube.Delete(ctx, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // never issued (no apiRef), or already gone
		}
		log.Error(err, "deleting the cdc's authn clientconfig Secret", "name", sec.GetName(), "namespace", sec.GetNamespace())
		return fmt.Errorf("deleting the cdc's authn clientconfig Secret %s/%s: %w", sec.GetNamespace(), sec.GetName(), err)
	}
	log.Debug("authn clientconfig Secret deleted", "name", sec.GetName(), "namespace", sec.GetNamespace())
	return nil
}

// hashAuthnServiceAccountMapping contributes deterministic inputs to the digest so Deploy and
// Lookup agree regardless of cluster state. No-op when no apiRef is declared.
func hashAuthnServiceAccountMapping(opts DeployOptions, saName string, hsh *hasher.ObjectHash) error {
	if opts.ApiRefName == "" {
		return nil
	}
	authnNS := authnNamespaceOrDefault(opts.AuthnNamespace)
	name := authnMappingName(opts.ControllerNamespace, saName)
	return hsh.SumHash(name, authnNS, cdcGroup(saName))
}

// applyAuthnServiceAccountMapping renders, applies, and hashes the mapping. No-op without apiRef.
func applyAuthnServiceAccountMapping(ctx context.Context, dyn dynamic.Interface, opts DeployOptions, saName, saNamespace string, hsh *hasher.ObjectHash, applyOpts kubecli.ApplyOptions) error {
	if opts.ApiRefName == "" {
		return nil
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	obj, err := renderAuthnServiceAccountMapping(opts, saName, saNamespace)
	if err != nil {
		return err
	}
	if err := kubecli.Apply(ctx, dyn, authnServiceAccountGVR, obj, applyOpts); err != nil {
		log.Error(err, "installing authn ServiceAccount mapping", "name", obj.GetName(), "namespace", obj.GetNamespace())
		return fmt.Errorf("installing authn ServiceAccount mapping: %w", err)
	}
	log.Debug("authn ServiceAccount mapping successfully installed", "name", obj.GetName(), "namespace", obj.GetNamespace())
	return hashAuthnServiceAccountMapping(opts, saName, hsh)
}

// ensureSelfAuthnMapping provisions core-provider's OWN authn allowlist mapping — the one that
// authorizes core-provider's pod ServiceAccount to exchange its projected token for the service
// JWT it presents to snowplow's JWT-gated /rbac. This is distinct from the per-CDC mapping
// (applyAuthnServiceAccountMapping): it is core-provider's own identity, needed the first time it
// calls /rbac.
//
// It is provisioned lazily here — the first time a composition declares an apiRef, by which point
// authn is running — rather than declaratively at bootstrap, where the
// serviceaccount.authn.krateo.io CRD does not yet exist (authn is installed later, as a
// component). The mapping's name is core-provider's own ServiceAccount name (stable, idempotent,
// and distinct from the "cdc-*" per-composition names). No-op when no apiRef is declared or the
// self-identity is not configured (apiRefRBAC disabled). Deliberately NOT hashed into the deploy
// digest: it is core-provider's singleton identity, not part of the per-composition resource set.
func ensureSelfAuthnMapping(ctx context.Context, dyn dynamic.Interface, opts DeployOptions, applyOpts kubecli.ApplyOptions) error {
	if opts.ApiRefName == "" || opts.SelfSAName == "" {
		return nil
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	authnNS := authnNamespaceOrDefault(opts.AuthnNamespace)
	group := opts.SelfGroup
	if group == "" {
		group = defaultSelfAuthnGroup
	}
	saNamespace := opts.SelfSANamespace
	if saNamespace == "" {
		saNamespace = authnNS
	}

	obj := &unstructured.Unstructured{}
	err := objects.CreateK8sObject(obj, opts.GVR,
		types.NamespacedName{Namespace: authnNS, Name: opts.SelfSAName},
		filepath.Join(opts.RBACFolderPath, authnServiceAccountTemplate),
		"saName", opts.SelfSAName,
		"saNamespace", saNamespace,
		"group", group,
		"displayName", "core-provider (RESTAction RBAC inspector)",
	)
	if err != nil {
		return fmt.Errorf("rendering core-provider authn ServiceAccount mapping: %w", err)
	}
	if err := kubecli.Apply(ctx, dyn, authnServiceAccountGVR, obj, applyOpts); err != nil {
		log.Error(err, "installing core-provider authn ServiceAccount mapping", "name", obj.GetName(), "namespace", obj.GetNamespace())
		return fmt.Errorf("installing core-provider authn ServiceAccount mapping: %w", err)
	}
	log.Debug("core-provider authn ServiceAccount mapping installed", "name", obj.GetName(), "namespace", obj.GetNamespace())
	return nil
}

// deleteAuthnServiceAccountMapping removes the mapping on undeploy. Not-found is ignored.
func deleteAuthnServiceAccountMapping(ctx context.Context, kube client.Client, authnNamespace, compositionNamespace, saName string) error {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(authnServiceAccountGVK)
	obj.SetNamespace(authnNamespaceOrDefault(authnNamespace))
	obj.SetName(authnMappingName(compositionNamespace, saName))

	if err := kube.Delete(ctx, obj); err != nil {
		// Not-found, or the authn CRD not being installed, must not block composition teardown.
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil
		}
		log.Error(err, "deleting authn ServiceAccount mapping", "name", obj.GetName(), "namespace", obj.GetNamespace())
		return fmt.Errorf("deleting authn ServiceAccount mapping: %w", err)
	}
	log.Debug("authn ServiceAccount mapping deleted", "name", obj.GetName(), "namespace", obj.GetNamespace())
	return nil
}
