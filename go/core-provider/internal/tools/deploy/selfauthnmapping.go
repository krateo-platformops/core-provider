package deploy

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// MappingState is what a live read said about one authn mapping. A read that FAILED is an error beside
// it, never a state: "could not look" must not be reported as "is not there" (#642).
type MappingState string

const (
	// MappingNotNeeded: nothing needs the mapping (no apiRef, or no self identity configured).
	MappingNotNeeded MappingState = ""
	// MappingPresent: the mapping exists and authorizes the ServiceAccount it is for.
	MappingPresent MappingState = "present"
	// MappingMissing: the cluster serves the mapping kind, and the mapping is absent or does not
	// authorize the ServiceAccount it is for.
	MappingMissing MappingState = "missing"
	// MappingNotInstalled: the cluster does not serve the mapping kind (no authn-crds there, yet).
	MappingNotInstalled MappingState = "notInstalled"
)

// SelfAuthnMapping is core-provider's OWN authn allowlist mapping: the one that authorizes
// core-provider's pod ServiceAccount to exchange its projected token for the service JWT it presents to
// snowplow's JWT-gated /rbac. It is distinct from the per-controller mapping
// (applyAuthnServiceAccountMapping): one per core-provider, not one per definition.
type SelfAuthnMapping struct {
	// SAName / SANamespace are core-provider's own ServiceAccount
	// (CORE_PROVIDER_APIREF_SELF_SA_NAME / _NAMESPACE). An empty SAName means apiRef RBAC is off in the
	// chart: there is no identity, and no mapping.
	SAName      string
	SANamespace string
	// Group is the identity's group (CORE_PROVIDER_APIREF_GROUP); empty is defaultSelfAuthnGroup.
	Group string
	// AuthnNamespace is where authn lists mappings; empty is defaultAuthnNamespace.
	AuthnNamespace string
	// RBACFolderPath holds the mapping template.
	RBACFolderPath string
}

func (m SelfAuthnMapping) render() (*unstructured.Unstructured, error) {
	authnNS := authnNamespaceOrDefault(m.AuthnNamespace)
	group := m.Group
	if group == "" {
		group = defaultSelfAuthnGroup
	}
	saNamespace := m.SANamespace
	if saNamespace == "" {
		saNamespace = authnNS
	}
	obj := &unstructured.Unstructured{}
	// The name is core-provider's own ServiceAccount name: stable, and distinct from the "cdc-*"
	// per-controller names. The template uses no GVR.
	err := objects.CreateK8sObject(obj, schema.GroupVersionResource{},
		types.NamespacedName{Namespace: authnNS, Name: m.SAName},
		filepath.Join(m.RBACFolderPath, authnServiceAccountTemplate),
		// No principal and no delegation: core-provider's own identity carries neither.
		authnMapping{saName: m.SAName, saNamespace: saNamespace, group: group,
			displayName: "core-provider (RESTAction RBAC inspector)"}.values()...,
	)
	if err != nil {
		return nil, fmt.Errorf("rendering core-provider's own authn mapping: %w", err)
	}
	return obj, nil
}

// EnsureSelfAuthnMapping makes core-provider's own mapping exist on hub, the management cluster, where
// core-provider and the authn it calls run, and reports what is there.
//
// It is NOT part of Deploy (#642). Deploy needs the mapping before it can do anything for a definition
// with an apiRef (the read-set it renders into RBAC is fetched with the identity the mapping
// authorizes), and Observe runs Deploy as a server-side dry run, which persists nothing. While Deploy
// wrote the mapping, a mapping that did not exist stayed missing: the dry run asked authn for a token,
// authn answered NoMapping, Observe failed, and the only real deploys, Create's and Update's, never ran
// again. So it is a precondition, ensured with a real write by whoever is about to call Deploy, dry run
// or not. It stays outside the deploy digest: it is core-provider's singleton, not part of any
// definition's resource set.
//
// It is provisioned at run time, not by the chart, because at bootstrap the mapping's CRD does not
// exist yet: authn is installed later, as a component. Until it is, the answer is MappingNotInstalled
// and no error, and the next pass looks again (nothing is cached).
//
// A write is sent only when the mapping is absent or its spec differs, so a steady-state pass only
// reads. Labels, annotations and the rest of a live object's metadata are kept: the spec is what this
// owns. mayWrite false (Observe of a definition its policy or a pause forbids acting on) only reads.
func EnsureSelfAuthnMapping(ctx context.Context, hub dynamic.Interface, m SelfAuthnMapping, mayWrite bool) (MappingState, error) {
	if m.SAName == "" {
		return MappingNotNeeded, nil
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())
	want, err := m.render()
	if err != nil {
		return "", err
	}

	live, err := hub.Resource(authnServiceAccountGVR).Namespace(want.GetNamespace()).Get(ctx, want.GetName(), metav1.GetOptions{})
	switch {
	case err == nil:
		if equality.Semantic.DeepEqual(live.Object["spec"], want.Object["spec"]) {
			return MappingPresent, nil
		}
		if !mayWrite {
			return authorizing(live, want), nil
		}
	case apierrors.IsNotFound(err):
		// NotFound is the object's or the whole kind's: only the CRD tells them apart.
		served, err := servesAuthnMapping(ctx, hub)
		if err != nil {
			return "", err
		}
		if !served {
			log.Info("authn is not installed on the management cluster; waiting for it to write core-provider's own authn mapping",
				"crd", authnMappingCRDName, "name", want.GetName(), "namespace", want.GetNamespace())
			return MappingNotInstalled, nil
		}
		if !mayWrite {
			return MappingMissing, nil
		}
	default:
		return "", fmt.Errorf("reading core-provider's own authn mapping %s/%s: %w", want.GetNamespace(), want.GetName(), err)
	}

	applyOpts := kubecli.ApplyOptions{FromCurrent: func(current, desired *unstructured.Unstructured) error {
		desired.Object["metadata"] = current.DeepCopy().Object["metadata"]
		return nil
	}}
	if _, err := applyRecorded(ctx, hub, authnServiceAccountGVR, authnServiceAccountGVK.Kind, auditrec.ActionAuthnMappingApply, want, applyOpts); err != nil {
		log.Error(err, "installing core-provider's own authn mapping", "name", want.GetName(), "namespace", want.GetNamespace())
		return "", fmt.Errorf("installing core-provider's own authn mapping: %w", err)
	}
	log.Info("core-provider's own authn mapping installed", "name", want.GetName(), "namespace", want.GetNamespace())
	return MappingPresent, nil
}

// ControllerAuthnMapping reads, where the controller runs, the mapping of the controller opts deploys
// (the pool's, for a pool member). It only reads: Deploy writes it.
func ControllerAuthnMapping(ctx context.Context, opts DeployOptions) (MappingState, error) {
	if opts.ApiRefName == "" {
		return MappingNotNeeded, nil
	}
	if opts.AuthnSupport.NotInstalled {
		return MappingNotInstalled, nil
	}
	nn, err := controllerObjectName(opts.ControllerNamespace, opts.GVR)
	if err != nil {
		return "", err
	}
	sa, _, _, _, _, err := createRBACResources(opts.GVR, getCDCrbacNN(nn), getCDCrbacNN(types.NamespacedName{Namespace: opts.Namespace, Name: nn.Name}), opts.RBACFolderPath)
	if err != nil {
		return "", err
	}
	sa, _ = controllerSubject(opts, sa)
	want, err := renderAuthnServiceAccountMapping(opts, sa.Name, sa.Namespace)
	if err != nil {
		return "", err
	}
	live, err := opts.DynClient.Resource(authnServiceAccountGVR).Namespace(want.GetNamespace()).Get(ctx, want.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return MappingMissing, nil
	case err != nil:
		return "", fmt.Errorf("reading the controller's authn mapping %s/%s: %w", want.GetNamespace(), want.GetName(), err)
	}
	return authorizing(live, want), nil
}

// authorizing is MappingPresent when live authorizes the same ServiceAccount, as the same groups, as
// want. Only those two fields: principal and delegation are allowed to differ, because the members of
// one controller pool share a mapping and each renders its own.
func authorizing(live, want *unstructured.Unstructured) MappingState {
	for _, f := range []string{"serviceAccountRef", "groups"} {
		l, _, _ := unstructured.NestedFieldNoCopy(live.Object, "spec", f)
		w, _, _ := unstructured.NestedFieldNoCopy(want.Object, "spec", f)
		if !equality.Semantic.DeepEqual(l, w) {
			return MappingMissing
		}
	}
	return MappingPresent
}

// servesAuthnMapping reports whether dyn's cluster serves the mapping kind: its CRD exists, is
// Established and serves the version core-provider writes. A CRD that cannot be read is an error.
func servesAuthnMapping(ctx context.Context, dyn dynamic.Interface) (bool, error) {
	crd, err := dyn.Resource(crdGVR).Get(ctx, authnMappingCRDName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s to tell a missing authn mapping from authn not being installed: %w", authnMappingCRDName, err)
	}
	return crdEstablished(crd) && servesVersion(crd, authnServiceAccountGVK.Version), nil
}

func crdEstablished(crd *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
	for _, c := range conds {
		if cm, ok := c.(map[string]interface{}); ok && cm["type"] == "Established" {
			return cm["status"] == "True"
		}
	}
	return false
}

func servesVersion(crd *unstructured.Unstructured, version string) bool {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		if vm, ok := v.(map[string]interface{}); ok && vm["name"] == version {
			served, _ := vm["served"].(bool)
			return served
		}
	}
	return false
}
