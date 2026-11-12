package deploy

import (
	"context"
	"fmt"
	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	"path/filepath"

	definitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	delegation, allowed, maxDepth := delegationValues(opts.ApiRefDelegation, opts.AuthnSupport)
	obj := &unstructured.Unstructured{}
	err := objects.CreateK8sObject(obj, opts.GVR,
		types.NamespacedName{Namespace: authnNS, Name: name},
		filepath.Join(opts.RBACFolderPath, authnServiceAccountTemplate),
		"saName", saName,
		"saNamespace", saNamespace,
		"group", cdcGroup(saName),
		"displayName", fmt.Sprintf("CDC (%s/%s)", saNamespace, saName),
		"principal", principalFor(opts.AuthnSupport),
		"delegation", delegation,
		"delegationAllowed", allowed,
		"delegationMaxDepth", maxDepth,
	)
	if err != nil {
		return nil, fmt.Errorf("rendering authn ServiceAccount mapping: %w", err)
	}
	return obj, nil
}

// hashAuthnServiceAccountMapping contributes deterministic inputs to the digest so Deploy and
// Lookup agree regardless of cluster state. No-op when no apiRef is declared.
func hashAuthnServiceAccountMapping(opts DeployOptions, saName string, hsh *hasher.ObjectHash) error {
	if opts.ApiRefName == "" {
		return nil
	}
	authnNS := authnNamespaceOrDefault(opts.AuthnNamespace)
	name := authnMappingName(opts.ControllerNamespace, saName)
	// What is rendered for principal and delegation (which depends on the served CRD) is an input to
	// the digest: declaring delegation, or authn-crds starting to serve a field, makes the definition
	// read as not up to date, and the Update re-renders the mapping (#405). Without them here, either
	// change would sit unapplied until something unrelated triggered an Update.
	delegation, allowed, maxDepth := delegationValues(opts.ApiRefDelegation, opts.AuthnSupport)
	return hsh.SumHash(name, authnNS, cdcGroup(saName), principalFor(opts.AuthnSupport), delegation, allowed, maxDepth)
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
	if _, err := applyRecorded(ctx, dyn, authnServiceAccountGVR, authnServiceAccountGVK.Kind, auditrec.ActionAuthnMappingApply, obj, applyOpts); err != nil {
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
	if _, err := applyRecorded(ctx, dyn, authnServiceAccountGVR, authnServiceAccountGVK.Kind, auditrec.ActionAuthnMappingApply, obj, applyOpts); err != nil {
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

// authnPrincipalController is the principal every composition controller's identity declares
// (authn spec.principal, enum agent|controller|service; unset means service).
const authnPrincipalController = "controller"

// authnMappingCRDName is the CRD of authn's ServiceAccount mapping.
const authnMappingCRDName = "serviceaccounts.serviceaccount.authn.krateo.io"

// AuthnSupport is which optional spec fields the INSTALLED authn ServiceAccount mapping CRD serves
// at the version core-provider writes. Writing a field the CRD does not serve is not an error: the
// CRD does not preserve unknown fields, so the apiserver PRUNES it on the full Update (at most a
// warning). The mapping would then silently differ from what the definition declares (#405, authn's
// review). So each field is rendered only when served, and a declared field the CRD can't carry is
// surfaced on the definition by the caller.
type AuthnSupport struct {
	// Principal: spec.principal (authn-crds >= 0.39.0).
	Principal bool
	// Delegation: spec.delegation (authn-crds >= 0.37.0).
	Delegation bool
}

// AuthnMappingSupport reads the installed mapping CRD's SERVED schema. The caller reads it once per
// reconcile, before rendering, rather than caching it at startup: a core-provider started before
// authn-crds gained a field picks it up on its next reconcile, no restart. It keys on the fields
// themselves, not on a chart version.
//
// A failed read is an error, not "unsupported": answering "none" on a transient error would
// re-render every mapping without principal and flap it on the next good read. A CRD that does not
// exist supports nothing (and the mapping apply fails on its own).
func AuthnMappingSupport(ctx context.Context, dyn dynamic.Interface) (AuthnSupport, error) {
	crd, err := dyn.Resource(crdGVR).Get(ctx, authnMappingCRDName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return AuthnSupport{}, nil
	}
	if err != nil {
		return AuthnSupport{}, fmt.Errorf("reading %s to decide which mapping fields it serves: %w", authnMappingCRDName, err)
	}
	return AuthnSupport{
		Principal:  servesSpecField(crd, authnServiceAccountGVK.Version, "principal"),
		Delegation: servesSpecField(crd, authnServiceAccountGVK.Version, "delegation"),
	}, nil
}

// principalFor is the spec.principal to render under support.
func principalFor(support AuthnSupport) string {
	if support.Principal {
		return authnPrincipalController
	}
	return ""
}

// servesSpecField reports whether crd SERVES version with spec.properties.<field> in its schema.
func servesSpecField(crd *unstructured.Unstructured, version, field string) bool {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		vm, ok := v.(map[string]interface{})
		if !ok || vm["name"] != version {
			continue
		}
		if served, _ := vm["served"].(bool); !served {
			return false
		}
		_, found, _ := unstructured.NestedMap(vm, "schema", "openAPIV3Schema", "properties", "spec", "properties", field)
		return found
	}
	return false
}

// delegationValues are the template inputs for spec.delegation: all "" when the definition declares
// none, or when the installed CRD does not serve spec.delegation (it would be pruned; the caller
// surfaces that on the definition) (the mapping then has no delegation key at all, so existing mappings render exactly as before
// #405), and maxDepth "" when unset (authn defaults it in code, not in its CRD, so it is not rendered).
func delegationValues(d *definitionsv1alpha1.ApiRefDelegation, support AuthnSupport) (delegation, allowed, maxDepth string) {
	if d == nil || !support.Delegation {
		return "", "", ""
	}
	allowed = "false"
	if d.Allowed {
		allowed = "true"
	}
	if d.MaxDepth != nil {
		maxDepth = fmt.Sprintf("%d", *d.MaxDepth)
	}
	return "true", allowed, maxDepth
}
