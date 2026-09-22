package compositiondefinitions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/status"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/core-provider/internal/tools/kube"
	pluralizerlib "github.com/krateo-platformops/core-provider/internal/tools/pluralizer"
	"github.com/krateo-platformops/core-provider/internal/tools/policy"
	coretelemetry "github.com/krateo-platformops/core-provider/internal/tools/telemetry"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/controller"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	record "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	errNotCR         = "managed resource is not a Definition custom resource"
	reconcileTimeout = 4 * time.Minute
)

var (
	CDCtemplateDeploymentPath       = filepath.Join(os.TempDir(), "assets/cdc-deployment/deployment.yaml")
	CDCtemplateConfigmapPath        = filepath.Join(os.TempDir(), "assets/cdc-configmap/configmap.yaml")
	CDCrbacConfigFolder             = filepath.Join(os.TempDir(), "assets/cdc-rbac/")
	JSONSchemaTemplateConfigmapPath = filepath.Join(os.TempDir(), "assets/json-schema-configmap/configmap.yaml")
	ServiceTemplatePath             = filepath.Join(os.TempDir(), "assets/cdc-service/service.yaml")

	// AuthnNamespace is the authn operator namespace where the per-composition ServiceAccount
	// allowlist mapping is created (when a CompositionDefinition declares an apiRef). Override
	// via COMPOSITION_AUTHN_NAMESPACE; defaults to "krateo-system".
	AuthnNamespace = envOr("COMPOSITION_AUTHN_NAMESPACE", "krateo-system")

	// SnowplowURL is snowplow's base URL. When a CompositionDefinition declares an apiRef,
	// core-provider calls snowplow's dispatch-free GET /rbac to enumerate the RESTAction's
	// read-set and grant it to the per-composition group. Override via CORE_PROVIDER_SNOWPLOW_URL;
	// required when apiRef is used (an empty value fails the reconcile with a clear message).
	SnowplowURL = envOr("CORE_PROVIDER_SNOWPLOW_URL", "")

	// AuthnURL is the authn service base URL. snowplow's /rbac is gated by the same JWT middleware
	// as /call, so core-provider exchanges its projected SA token for an authn-issued service JWT
	// to authenticate. Override via CORE_PROVIDER_AUTHN_URL.
	AuthnURL = envOr("CORE_PROVIDER_AUTHN_URL", "")

	// SelfSAName / SelfSANamespace / SelfGroup identify core-provider's OWN ServiceAccount for its
	// apiRefRBAC authn allowlist mapping. It is provisioned at runtime (the first time a
	// composition declares an apiRef) rather than declaratively at bootstrap, where the authn CRD
	// does not yet exist. The chart sets these when apiRefRBAC is enabled; empty SelfSAName
	// disables self-provisioning.
	SelfSAName      = envOr("CORE_PROVIDER_APIREF_SELF_SA_NAME", "")
	SelfSANamespace = envOr("CORE_PROVIDER_APIREF_SELF_SA_NAMESPACE", "")
	SelfGroup       = envOr("CORE_PROVIDER_APIREF_GROUP", "krateo:core-provider")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type Options struct {
	ControllerOptions controller.Options
	// Metrics records reconcile telemetry for the CompositionDefinition controller.
	Metrics    reconciler.MetricsRecorder
	Pluralizer pluralizerlib.PluralizerInterface
}

func Setup(mgr ctrl.Manager, o Options) error {
	name := reconciler.ControllerName(compositiondefinitionsv1alpha1.CompositionDefinitionGroupKind)

	l := o.ControllerOptions.Logger.WithValues("controller", name)

	throttledRecorder, err := eventrecorder.CreateWithThrottle(context.Background(), mgr.GetConfig(), name, nil)
	if err != nil {
		return fmt.Errorf("error creating event recorder: %w", err)
	}

	recorder, err := eventrecorder.Create(context.Background(), mgr.GetConfig(), name, nil)
	if err != nil {
		return fmt.Errorf("error creating event recorder: %w", err)
	}

	cli := mgr.GetClient()

	// Cleanup: Remove obsolete label for backward compatibility on startup
	// This handles CompositionDefinitions created before the removal of the still-exist-compositions-finalizer
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cleanupCancel()
	if err := cleanupObsoleteFinalizerLabels(cleanupCtx, cli, l); err != nil {
		l.Debug("Failed to cleanup obsolete finalizer labels on startup", "error", err)
	}

	// core-provider hosts no admission webhooks: generated CRDs use None conversion, and
	// the composition-version label is stamped by a MutatingAdmissionPolicy that must exist
	// in every cluster a composition CRD lives in (requires Kubernetes >= 1.36). On the
	// management cluster the chart ships it; for remote targets core-provider projects it
	// into the target during bootstrap (see external.ensureCompositionVersionPolicy).

	r := reconciler.NewReconciler(mgr,
		resource.ManagedKind(compositiondefinitionsv1alpha1.CompositionDefinitionGroupVersionKind),
		reconciler.WithExternalConnecter(&connector{
			client:     kubernetes.NewForConfigOrDie(mgr.GetConfig()),
			dynamic:    dynamic.NewForConfigOrDie(mgr.GetConfig()),
			kube:       cli,
			apiReader:  mgr.GetAPIReader(),
			log:        l,
			recorder:   recorder,
			pluralizer: o.Pluralizer,
		}),
		reconciler.WithTimeout(reconcileTimeout),
		reconciler.WithPollInterval(o.ControllerOptions.PollInterval),
		reconciler.WithLogger(l),
		reconciler.WithMetrics(o.Metrics),
		reconciler.WithRecorder(event.NewAPIRecorder(recorder)),
		reconciler.WithThrottledRecorder(event.NewAPIRecorder(throttledRecorder)),
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ControllerOptions.ForControllerRuntime()).
		For(&compositiondefinitionsv1alpha1.CompositionDefinition{}).
		// Re-reconcile a CompositionDefinition when a Secret it references changes (its
		// chart credentials, or a kubeconfig Secret behind its KubernetesTarget), so
		// credentials rotated out-of-band (e.g. by External Secrets Operator) are picked
		// up promptly instead of waiting for the next poll.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(enqueueForReferencedSecret(cli))).
		// Re-reconcile CompositionDefinitions when the KubernetesTarget they reference
		// changes (e.g. its kubeconfigRef is repointed).
		Watches(&compositiondefinitionsv1alpha1.KubernetesTarget{}, handler.EnqueueRequestsFromMapFunc(enqueueForKubernetesTarget(cli))).
		Complete(ratelimiter.New(name, r, o.ControllerOptions.GlobalRateLimiter))
}

// enqueueForReferencedSecret maps a Secret event to reconcile requests for every
// CompositionDefinition that references it - directly as chart credentials, or
// transitively via a KubernetesTarget whose kubeconfigRef points at the Secret.
func enqueueForReferencedSecret(cli client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			return nil
		}

		// Namespaced keys (target-namespace/target-name) of KubernetesTargets whose kubeconfig
		// lives in this Secret. A CompositionDefinition resolves its targetRef in its OWN
		// namespace, so the match must be on (namespace, name), not name alone.
		targets := map[targetKey]bool{}
		var targetList compositiondefinitionsv1alpha1.KubernetesTargetList
		if err := cli.List(ctx, &targetList); err == nil {
			for i := range targetList.Items {
				ref := targetList.Items[i].Spec.KubeconfigRef
				if ref.Namespace == secret.Namespace && ref.Name == secret.Name {
					targets[targetKey{namespace: targetList.Items[i].Namespace, name: targetList.Items[i].Name}] = true
				}
			}
		}

		var list compositiondefinitionsv1alpha1.CompositionDefinitionList
		if err := cli.List(ctx, &list); err != nil {
			return nil
		}

		var reqs []reconcile.Request
		for i := range list.Items {
			cd := &list.Items[i]
			if compositionReferencesChartSecret(cd, secret.Namespace, secret.Name) ||
				compositionReferencesTargetIn(cd, targets) {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cd)})
			}
		}
		return reqs
	}
}

// enqueueForKubernetesTarget maps a KubernetesTarget event to reconcile requests for
// every CompositionDefinition referencing it.
func enqueueForKubernetesTarget(cli client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		kt, ok := obj.(*compositiondefinitionsv1alpha1.KubernetesTarget)
		if !ok {
			return nil
		}

		var list compositiondefinitionsv1alpha1.CompositionDefinitionList
		if err := cli.List(ctx, &list); err != nil {
			return nil
		}

		var reqs []reconcile.Request
		for i := range list.Items {
			cd := &list.Items[i]
			if compositionReferencesTargetIn(cd, map[targetKey]bool{{namespace: kt.Namespace, name: kt.Name}: true}) {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cd)})
			}
		}
		return reqs
	}
}

// compositionReferencesChartSecret reports whether cd uses the Secret ns/name as its
// chart repository credentials.
func compositionReferencesChartSecret(cd *compositiondefinitionsv1alpha1.CompositionDefinition, ns, name string) bool {
	c := cd.Spec.Chart
	if c == nil || c.Credentials == nil {
		return false
	}
	return c.Credentials.PasswordRef.Namespace == ns && c.Credentials.PasswordRef.Name == name
}

// targetKey identifies a KubernetesTarget by its namespace and name. targetRefs resolve
// same-namespace, so a CompositionDefinition maps to a KubernetesTarget only when their
// namespaces match.
type targetKey struct {
	namespace string
	name      string
}

// compositionReferencesTargetIn reports whether cd's deploy.targetRef names one of the
// given KubernetesTargets. The match is on (namespace, name): cd's targetRef resolves in
// cd's OWN namespace, so a same-named target in a different namespace is not a match.
func compositionReferencesTargetIn(cd *compositiondefinitionsv1alpha1.CompositionDefinition, targets map[targetKey]bool) bool {
	d := cd.Spec.Deploy
	if d == nil || d.TargetRef == nil {
		return false
	}
	return targets[targetKey{namespace: cd.Namespace, name: d.TargetRef.Name}]
}

// cleanupObsoleteFinalizerLabels removes the obsolete "composition.krateo.io/still-exist-compositions-finalizer" label
// from all CompositionDefinitions for backward compatibility. This handles resources created before the label was removed.
func cleanupObsoleteFinalizerLabels(ctx context.Context, kube client.Client, log logging.Logger) error {
	const obsoleteLabel = "composition.krateo.io/still-exist-compositions-finalizer"

	list := &compositiondefinitionsv1alpha1.CompositionDefinitionList{}
	if err := kube.List(ctx, list); err != nil {
		return fmt.Errorf("error listing CompositionDefinitions: %w", err)
	}

	if len(list.Items) == 0 {
		log.Debug("No CompositionDefinitions found for cleanup")
		return nil
	}

	cleaned := 0
	for i := range list.Items {
		cr := &list.Items[i]
		if cr.Labels != nil {
			if _, exists := cr.Labels[obsoleteLabel]; exists {
				delete(cr.Labels, obsoleteLabel)
				if err := kube.Update(ctx, cr); err != nil {
					log.Debug("Failed to remove obsolete finalizer label", "name", cr.Name, "namespace", cr.Namespace, "error", err)
					continue
				}
				cleaned++
				log.Debug("Removed obsolete finalizer label", "name", cr.Name, "namespace", cr.Namespace)
			}
		}
	}

	if cleaned > 0 {
		log.Info("Cleanup completed", "removed_labels", cleaned)
	}
	return nil
}

type connector struct {
	dynamic dynamic.Interface
	client  kubernetes.Interface
	kube    client.Client
	// apiReader reads directly from the API server (bypassing the controller-runtime cache) so
	// Observe can re-read the CompositionDefinition fresh — see external.apiReader / D3.
	apiReader  client.Reader
	log        logging.Logger
	recorder   record.EventRecorder
	pluralizer pluralizerlib.PluralizerInterface
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (reconciler.ExternalClient, error) {
	cr, ok := mg.(*compositiondefinitionsv1alpha1.CompositionDefinition)
	if !ok {
		return nil, fmt.Errorf(errNotCR)
	}

	log := c.log.WithValues("name", cr.Name, "namespace", cr.Namespace)

	ext := &external{
		mgmt:        c.kube,
		mgmtDynamic: c.dynamic,
		apiReader:   c.apiReader,
		kube:        c.kube,
		dynamic:     c.dynamic,
		client:      c.client,
		log:         log,
		rec:         c.recorder,
		pluralizer:  c.pluralizer,
	}

	// When the CompositionDefinition targets a remote cluster, the generated CRD, its
	// RBAC and the composition-dynamic-controller are deployed there. The
	// CompositionDefinition resource and its referenced secrets stay in the management
	// cluster, so mgmt keeps pointing at the local cluster.
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		// The KubernetesTarget is namespaced and resolved in the CompositionDefinition's
		// own namespace.
		tc, err := clusterkube.Remote(ctx, c.kube, cr.Namespace, cr.Spec.Deploy)
		if err != nil {
			return nil, err
		}
		ext.kube = tc.Kube
		ext.dynamic = tc.Dynamic
		ext.client = tc.Clientset
		ext.remote = true
		ext.secretResourceVersion = tc.SecretResourceVersion
		log.Debug("Deploying to remote target cluster", "host", tc.Config.Host)
	}

	return ext, nil
}

type external struct {
	// mgmt is the management-cluster client: it holds the CompositionDefinition
	// resource, the chart/credentials secrets, and is where status is persisted.
	mgmt client.Client
	// mgmtDynamic is the hub (management-cluster) dynamic client. Unlike dynamic (below), which
	// Connect swaps to the spoke for a remote CD, mgmtDynamic always points at the hub. It lets the
	// remote branch apply the generated composition CRD onto the hub as well as the spoke, so the
	// desired Composition can be authored/validated on the hub
	// (docs/design/remote-composition-mirror.md).
	mgmtDynamic dynamic.Interface
	// apiReader reads the CompositionDefinition straight from the API server, bypassing the
	// controller-runtime cache. Observe re-reads the CR through it so a lagging informer cache
	// cannot make the engine reconcile a stale spec.chart.version (D3).
	apiReader client.Reader
	// kube, dynamic and client target the cluster where the generated CRD, RBAC and the
	// composition-dynamic-controller are deployed (local == mgmt, or a remote target).
	dynamic    dynamic.Interface
	kube       client.Client
	client     kubernetes.Interface
	log        logging.Logger
	rec        record.EventRecorder
	pluralizer pluralizerlib.PluralizerInterface

	// remote is true when the target is a remote cluster; secretResourceVersion is the
	// resourceVersion of the kubeconfig Secret used to reach it.
	remote                bool
	secretResourceVersion string
}

// setTargetStatus records where the controller is deployed and whether that cluster is
// reachable, by probing the target cluster's discovery endpoint.
func (e *external) setTargetStatus(cr *compositiondefinitionsv1alpha1.CompositionDefinition) {
	mode := compositiondefinitionsv1alpha1.DeploymentModeLocal
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		mode = compositiondefinitionsv1alpha1.DeploymentModeRemote
	}

	ts := &compositiondefinitionsv1alpha1.TargetStatus{Mode: string(mode)}
	if v, err := e.client.Discovery().ServerVersion(); err == nil {
		ts.ConnectionStatus = "Healthy"
		ts.Version = v.GitVersion
	} else {
		ts.ConnectionStatus = "Down"
	}
	if e.remote {
		ts.KubeconfigSecretResourceVersion = e.secretResourceVersion
	}

	cr.Status.Target = ts
}

// ensureCompositionVersionPolicy projects the cluster-wide composition-version
// MutatingAdmissionPolicy into a remote target. The label it stamps is what per-version
// listing/migration and safe deletion rely on; for local targets the management chart
// already ships the policy, so this only acts on remote targets. Create-if-absent, so it
// never fights a chart- or operator-managed policy already present in the target.
func (e *external) ensureCompositionVersionPolicy(ctx context.Context) error {
	if !e.remote {
		return nil
	}
	if err := policy.EnsureCompositionVersionPolicy(ctx, e.kube); err != nil {
		return fmt.Errorf("error projecting composition-version policy into target: %w", err)
	}
	return nil
}

// versionReferencedByAnotherDefinition reports whether any CompositionDefinition OTHER than
// (selfName/selfNamespace) currently targets the given generated GVK (group+kind+version). It
// is the reference count behind safe version retirement: a per-(CRD,version) dynamic controller
// is shared by every definition on that version, so it may only be torn down once no other
// definition still needs it.
func (e *external) versionReferencedByAnotherDefinition(ctx context.Context, gvk schema.GroupVersionKind, selfName, selfNamespace string) (bool, error) {
	cds, err := getters.GetCompositionDefinitionsWithVersion(ctx, e.mgmt, gvk)
	if err != nil {
		return false, err
	}
	for i := range cds {
		if cds[i].Name == selfName && cds[i].Namespace == selfNamespace {
			continue
		}
		return true, nil
	}
	return false, nil
}

// prunableServedVersions inspects the live CRD and returns the served versions that are safe to
// prune: not the current version (gvk.Version), not "vacuum" (the storage version), not referenced
// by any OTHER CompositionDefinition, and with no Composition carrying their
// krateo.io/composition-version label. SHARED by Observe (to report not-up-to-date so Update
// re-runs the prune to completion) and pruneStaleServedVersions — using the SAME predicate for the
// drive condition and the actual prune is what prevents an Observe<->Update ping-pong. `kept` is the
// per-version keep reasons (logging only).
func (e *external) prunableServedVersions(ctx context.Context, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource, selfName, selfNamespace string) (prunable, kept []string, err error) {
	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return nil, nil, fmt.Errorf("fetching CRD: %w", err)
	}
	if crd == nil {
		return nil, nil, nil
	}
	for _, v := range crd.Spec.Versions {
		if v.Name == "vacuum" || v.Name == gvk.Version {
			continue // never the storage version or the current served version
		}
		referenced, refErr := e.versionReferencedByAnotherDefinition(ctx, schema.GroupVersionKind{
			Group: gvk.Group, Kind: gvk.Kind, Version: v.Name,
		}, selfName, selfNamespace)
		if refErr != nil {
			return nil, nil, fmt.Errorf("checking references for version %s: %w", v.Name, refErr)
		}
		if referenced {
			kept = append(kept, v.Name+":referenced")
			continue // another definition is still on this version
		}
		insts, listErr := getters.GetCompositionsByVersionLabel(ctx, e.dynamic,
			schema.GroupVersionResource{Group: gvr.Group, Version: v.Name, Resource: gvr.Resource}, v.Name)
		if listErr != nil {
			return nil, nil, fmt.Errorf("listing instances for version %s: %w", v.Name, listErr)
		}
		if insts != nil && len(insts.Items) > 0 {
			kept = append(kept, fmt.Sprintf("%s:instances=%d", v.Name, len(insts.Items)))
			continue // an instance still carries this version's label
		}
		prunable = append(prunable, v.Name)
	}
	return prunable, kept, nil
}

// pruneStaleServedVersions removes the prune-eligible served versions (per prunableServedVersions)
// from the generated CRD, so a CRD does not accumulate every version hop's served version forever
// (#103) — dead weight, and a stale served endpoint a client can pick that silently prunes fields
// written through it. The current version and vacuum (storage) are always kept. Applied via
// kube.Apply (get-fresh-resourceVersion + retry-on-conflict), not a bare client Update, because
// right after a new version is appended the apiserver churns the CRD status (acceptedNames /
// per-version established conditions) and a non-retrying Update would lose the resourceVersion race.
// Driven to completion by the matching check in Observe.
func (e *external) pruneStaleServedVersions(ctx context.Context, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource, selfName, selfNamespace string) error {
	prunable, kept, err := e.prunableServedVersions(ctx, gvk, gvr, selfName, selfNamespace)
	if err != nil {
		return err
	}
	// INFO (not Debug) so the prune decision is visible in the engine's default log level (#103).
	e.log.Info("served-version prune evaluation", "gvr", gvr.String(), "current", gvk.Version,
		"prunable", prunable, "kept", kept)
	if len(prunable) == 0 {
		return nil
	}
	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return fmt.Errorf("fetching CRD for prune: %w", err)
	}
	pruneSet := map[string]bool{}
	for _, v := range prunable {
		pruneSet[v] = true
	}
	// The apiserver forbids removing a version from spec.versions while it is still listed in
	// status.storedVersions (a version that was, at some point, a storage version — e.g. the very
	// first served version before "vacuum" became the permanent storage version). Trim the pruned
	// versions out of storedVersions FIRST, via the status subresource, then remove them from
	// spec.versions. Safe: the migration loop re-writes every Composition through the current served
	// endpoint, so all data is now persisted at the current storage version ("vacuum") — none remains
	// at these retired versions. Without this the whole spec update is rejected and nothing prunes.
	var keptStored []string
	for _, sv := range crd.Status.StoredVersions {
		if !pruneSet[sv] {
			keptStored = append(keptStored, sv)
		}
	}
	if len(keptStored) != len(crd.Status.StoredVersions) {
		crd.Status.StoredVersions = keptStored
		if err := e.kube.Status().Update(ctx, crd); err != nil {
			return fmt.Errorf("trimming storedVersions before prune: %w", err)
		}
	}
	if !crdutils.RemoveStaleVersions(crd, pruneSet) {
		return nil
	}
	if err := kube.Apply(ctx, e.dynamic, apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"), crd, kube.ApplyOptions{}); err != nil {
		return fmt.Errorf("applying pruned CRD: %w", err)
	}
	e.log.Info("Pruned stale served versions from CRD", "gvr", gvr.String(), "pruned", prunable)
	return nil
}

// encodeStatusDataTemplate serializes the CompositionDefinition's statusDataTemplate to the
// engine's JSON wire format ([{forPath,expression}]) for delivery to the CDC via its
// ConfigMap (COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE). Empty when nothing is declared.
func encodeStatusDataTemplate(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if len(cr.Spec.StatusDataTemplate) == 0 {
		return ""
	}
	type wire struct {
		ForPath    string `json:"forPath"`
		Expression string `json:"expression"`
	}
	ws := make([]wire, 0, len(cr.Spec.StatusDataTemplate))
	for i := range cr.Spec.StatusDataTemplate {
		m := &cr.Spec.StatusDataTemplate[i]
		ws = append(ws, wire{ForPath: m.ForPath, Expression: m.Expression})
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return ""
	}
	return string(b)
}

// encodeApiRefExtras serializes the CompositionDefinition's apiRef.extras (the inline,
// author-declared static map, snowplow spec.apiRef.extras) to a compact JSON object for
// delivery to the CDC via COMPOSITION_CONTROLLER_API_REF_EXTRAS. Empty when no apiRef or no
// extras are declared.
func encodeApiRefExtras(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if cr.Spec.ApiRef == nil || cr.Spec.ApiRef.Extras == nil {
		return ""
	}
	// Extras is an apiextensionsv1.JSON; its Raw is already the JSON encoding.
	return string(cr.Spec.ApiRef.Extras.Raw)
}

// apiRefName / apiRefNamespace return the referenced RESTAction coordinates, or "" when no
// apiRef is declared (which disables ".api" resolution in the CDC).
func apiRefName(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if cr.Spec.ApiRef == nil {
		return ""
	}
	return cr.Spec.ApiRef.Name
}

func apiRefNamespace(cr *compositiondefinitionsv1alpha1.CompositionDefinition) string {
	if cr.Spec.ApiRef == nil {
		return ""
	}
	return cr.Spec.ApiRef.Namespace
}

// statusFieldsFromSpec maps the CompositionDefinition's statusDataTemplate declarations to
// the generation package's decoupled StatusField list (used to validate and to inject the
// declared properties into the generated CRD's status schema).
func statusFieldsFromSpec(cr *compositiondefinitionsv1alpha1.CompositionDefinition) []crdutils.StatusField {
	fields := make([]crdutils.StatusField, 0, len(cr.Spec.StatusDataTemplate))
	for i := range cr.Spec.StatusDataTemplate {
		m := &cr.Spec.StatusDataTemplate[i]
		fields = append(fields, crdutils.StatusField{
			ForPath:               m.ForPath,
			Expression:            m.Expression,
			Type:                  m.Type,
			Schema:                m.Schema,
			PreserveUnknownFields: m.PreserveUnknownFields,
		})
	}
	return fields
}

// seedReadyIfAbsent gives a CompositionDefinition a Ready condition before any work that can fail.
//
// Observe's error paths return bare, so a definition that fails before anything sets Ready ends up
// carrying Synced=False and NO Ready condition at all. That is worse than Ready=False: a sweep
// filtering `Ready != True` catches it, but one filtering `Ready == False` reports a clean fleet,
// and to a human an absent condition reads as "still starting" rather than "broken". On 057 a
// definition sat failing its chart fetch for FIVE DAYS in exactly this shape without surfacing.
//
// Nor is it self-correcting: the first failure decides. Every later reconcile takes the same early
// return, so the field is never backfilled.
//
// This survives that first failure because the reconciler persists conditions even when Observe
// returns an error — it sets ReconcileError and calls Status().Update before propagating.
//
// Only seeds when genuinely absent. GetCondition returns a zero-Reason placeholder for a condition
// that is not there, while every condition this controller sets carries a Reason, so an existing
// verdict — including Ready=True — is never disturbed.
func seedReadyIfAbsent(cr *compositiondefinitionsv1alpha1.CompositionDefinition) {
	if cr.GetCondition(rtv1.TypeReady).Reason == "" {
		cr.SetConditions(rtv1.Creating())
	}
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (reconciler.ExternalObservation, error) {
	cr, ok := mg.(*compositiondefinitionsv1alpha1.CompositionDefinition)
	if !ok {
		return reconciler.ExternalObservation{}, fmt.Errorf(errNotCR)
	}
	// Engine reconcile span. core-provider is the top of the chain (no inbound traceparent);
	// the OTel log handler (provider-runtime logging.NewOTelHandler) bridges this span's
	// trace_id/span_id onto every log of the reconcile. No-op tracer until OTEL_TRACING_ENABLED.
	ctx, span := coretelemetry.Tracer().Start(ctx, "compositiondefinition.observe",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("k8s.object.name", cr.GetName()),
			attribute.String("k8s.object.namespace", cr.GetNamespace()),
		),
	)
	defer span.End()
	log := e.log.WithValues("operation", "observe")
	ctx = contexttools.CtxWithLogger(ctx, log)

	// D3 (2026-07-08): the managed reconciler hands us the CompositionDefinition from the
	// controller-runtime cache. If that cache lags a spec.chart.version bump (a missed/late watch),
	// every input below — the chart fetch (chartfs.ForSpec reads cr.Spec.Chart), the derived GVK/GVR,
	// the generated-CRD version — is computed from the STALE version, so the engine keeps regenerating
	// the OLD CRD version and only self-heals at the next full re-list (SyncPeriod) or a manual engine
	// restart. Re-read the CR straight from the API server so the version decision always uses the
	// current spec. Best-effort: on error we keep the cached copy rather than fail the reconcile.
	if e.apiReader != nil {
		fresh := cr.DeepCopy()
		if err := e.apiReader.Get(ctx, client.ObjectKeyFromObject(cr), fresh); err != nil {
			log.Debug("fresh CR re-read failed; using cached copy", "error", err)
		} else {
			fresh.DeepCopyInto(cr)
		}
	}
	deleted := meta.WasDeleted(cr)

	log.Info("Observing CompositionDefinition")

	// Record where the controller is deployed and whether that cluster is reachable.
	e.setTargetStatus(cr)

	// Seed Ready=False before any of the work that can fail. Every error path below returns bare,
	// so a definition that fails before anything sets Ready ends up carrying Synced=False and NO
	// Ready condition at all. That is a far worse state than Ready=False: a sweep filtering
	// `Ready != True` catches it, but one filtering `Ready == False` reports a clean fleet, and to
	// a human a missing condition reads as "still starting" rather than "broken". On 057 a
	// definition sat failing its chart fetch for FIVE DAYS in exactly this shape without surfacing.
	//
	// It is not self-correcting either: the first failure decides. Every later reconcile takes the
	// same early return, so the field is never backfilled.
	//
	// This survives the first failure because the reconciler persists conditions even when Observe
	// returns an error — it sets ReconcileError and calls Status().Update before propagating.
	//
	seedReadyIfAbsent(cr)

	pkgInfo, dir, err := chart.ChartInfoFromSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error getting chart info: %w", err)
	}

	pkg, err := chartfs.ForSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return reconciler.ExternalObservation{}, err
	}

	chartGVK, err := chartfs.GroupVersionKind(pkg)
	if err != nil {
		return reconciler.ExternalObservation{}, err
	}
	specSchemaBytes, err := chart.ChartJsonSchema(pkgInfo, dir)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error getting spec schema: %w", err)
	}

	gvr, err := e.pluralizer.GVKtoGVR(chartGVK)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return reconciler.ExternalObservation{}, fmt.Errorf("error converting GVK to GVR: %w - GVK: %s", err, chartGVK.String())
		}
		// The pluralizer resolves against the MANAGEMENT cluster. A remote target's generated CRD
		// lives on the spoke, so discovery misses it here — derive the GVR from the generated-CRD
		// schema and let the CRD lookup below (against e.kube, which is the spoke for remote targets)
		// decide whether the external resource actually exists. Previously a CD *being deleted* took
		// a shortcut to ResourceExists=false whenever the hub pluralizer missed, which for a remote
		// target skipped external.Delete entirely and orphaned the spoke's cdc, generated CRD and
		// composition instances.
		gvr, err = crdutils.GetGVRFromGeneratedCRD(specSchemaBytes, chartGVK)
		if err != nil {
			return reconciler.ExternalObservation{}, fmt.Errorf("error getting GVR from generated CRD for GVR fallback: %w", err)
		}
	} else if deleted {
		log.Debug("CompositionDefinition was deleted, CRD still resolves; continuing observation", "gvr", gvr.String())
	}

	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error getting CRD: %w", err)
	}
	if crd == nil {
		log.Debug("CRD not found", "gvr", gvr.String())
		cr.SetConditions(rtv1.Unavailable().
			WithMessage(fmt.Sprintf("crd for '%s' does not exists yet", gvr.String())))
		return reconciler.ExternalObservation{
			ResourceExists:   false,
			ResourceUpToDate: false,
		}, nil
	}

	existVersion, err := crdclient.Lookup(ctx, e.kube, gvr)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error looking up existing CRD version: %w", err)
	}
	if !existVersion {
		log.Debug("CRD version not found", "gvr", gvr.String())
		cr.SetConditions(rtv1.Unavailable().
			WithMessage(fmt.Sprintf("crd for '%s' does not exists yet", gvr.String())))
		return reconciler.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	genCRD, err := crdutils.GenerateCRD(specSchemaBytes, chartGVK)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error generating CRD: %w", err)
	}

	statusFields := statusFieldsFromSpec(cr)
	if err := crdutils.ValidateStatusFields(statusFields); err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("invalid statusDataTemplate: %w", err)
	}
	if err := crdutils.InjectStatusFields(genCRD, statusFields); err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error injecting declared status fields: %w", err)
	}

	statusChanged, err := crdutils.StatusEqual(crd, genCRD)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error comparing CRD status: %w", err)
	}

	if !statusChanged {
		log.Debug("CRD status changed", "gvr", gvr.String())
		return reconciler.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	// Certificate management is now handled by a separate CertificateReconciler
	// that runs independently on a periodic schedule.

	ul, err := getters.GetCompositions(ctx, e.dynamic, gvr)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error getting compositions: %w", err)
	}
	if len(ul.Items) > 0 {
		log.Debug("Compositions exist for this definition", "count", len(ul.Items))
	}

	log.Debug("Searching for Dynamic Controller", "gvr", gvr)

	// Seed the remote target before the dry-run deploy below: it projects the cdc into cr.Namespace
	// on the spoke, so that namespace must exist there, and the shadow CompositionDefinition must be
	// present for the projected cdc to resolve its package. Idempotent; no-op for local deployments.
	if err := e.seedRemoteTargetIfNeeded(ctx, cr, gvr, chartGVK); err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error seeding remote target: %w", err)
	}

	opts := deploy.DeployOptions{
		RBACFolderPath:         CDCrbacConfigFolder,
		DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
		KubeClient:             e.kube,
		Namespace:              cr.Namespace,
		GVR:                    gvr,
		Spec:                   cr.Spec.Chart.DeepCopy(),
		Controller:             cr.Spec.Controller.DeepCopy(),
		DeploymentTemplatePath: CDCtemplateDeploymentPath,
		ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
		JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
		JsonSchemaBytes:        specSchemaBytes,
		ServiceTemplatePath:    ServiceTemplatePath,
		DynClient:              e.dynamic,
		StatusDataTemplate:     encodeStatusDataTemplate(cr),
		ApiRefName:             apiRefName(cr),
		ApiRefNamespace:        apiRefNamespace(cr),
		ApiRefExtras:           encodeApiRefExtras(cr),
		AuthnNamespace:         AuthnNamespace,
		SnowplowURL:            SnowplowURL,
		AuthnURL:               AuthnURL,
		SelfSAName:             SelfSAName,
		SelfSANamespace:        SelfSANamespace,
		SelfGroup:              SelfGroup,
		DryRunServer:           true,
	}
	dig, err := deploy.Deploy(ctx, e.kube, opts)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error deploying dynamic controller in dry-run mode: %w", err)
	}

	if cr.Status.Digest != dig {
		log.Debug("Rendered resources digest changed", "status", cr.Status.Digest, "rendered", dig)
		return reconciler.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	dig, err = deploy.Lookup(ctx, e.kube, opts)
	if err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error looking up deployed resources digest: %w", err)
	}
	if cr.Status.Digest != dig {
		log.Debug("Deployed resources digest changed", "status", cr.Status.Digest, "deployed", dig)
		return reconciler.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	// Drive version migration to completion: if any composition still carries a previous
	// served version's label, report not-up-to-date so Update re-runs and re-stamps it.
	// The composition-version policy keys off the write endpoint, so a straggler can
	// survive a prior transition; without this the migration would be one-shot and the
	// straggler stays orphaned (the new label-scoped controller never selects it).
	// Only demand migration when UpgradePolicy approves it (4.1). Under Manual (without the
	// upgrade-to-version annotation) or Paused, existing instances legitimately coexist on their old
	// version, so their still-present label must NOT drive not-up-to-date — otherwise Observe would
	// forever re-drive an Update that deliberately declines to migrate (Observe<->Update ping-pong)
	// and the CompositionDefinition would never reach Available. This mirrors the Update-side gate.
	if cr.MigrationApproved(gvr.Version) {
		observeOwner := getters.DefinitionRef{Name: cr.Name, Namespace: cr.Namespace}
		for _, vi := range cr.Status.Managed.VersionInfo {
			if vi.Version == gvr.Version {
				continue
			}
			// List THROUGH the current served endpoint, selecting by the old version LABEL
			// (the label, not the served apiVersion, identifies the owning controller) and scoped
			// to THIS definition — another definition's instances on this version are not ours.
			stale, err := getters.GetOwnedCompositionsByVersionLabel(ctx, e.dynamic, gvr, vi.Version, observeOwner)
			if err != nil {
				return reconciler.ExternalObservation{}, fmt.Errorf("error checking compositions on version %s: %w", vi.Version, err)
			}
			if len(stale.Items) > 0 {
				log.Debug("Compositions pending version migration", "fromVersion", vi.Version, "toVersion", gvr.Version, "count", len(stale.Items))
				return reconciler.ExternalObservation{
					ResourceExists:   true,
					ResourceUpToDate: false,
				}, nil
			}
		}
	}

	// Drive served-version pruning to completion: if the live CRD still carries a prune-eligible
	// served version (the SAME predicate pruneStaleServedVersions uses), report not-up-to-date so
	// Update re-runs and removes it. Without this the prune is one-shot — Observe goes up-to-date
	// after migration completes (its gates key off stale COMPOSITIONS, status hash and digest, never
	// the set of stale served VERSIONS) before Update ever prunes, so stale served versions would
	// accumulate forever (#103). Same predicate on both sides => no Observe<->Update ping-pong.
	if prunable, _, perr := e.prunableServedVersions(ctx, chartGVK, gvr, cr.Name, cr.Namespace); perr != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error checking prunable served versions: %w", perr)
	} else if len(prunable) > 0 {
		log.Debug("Stale served versions pending prune", "versions", prunable)
		return reconciler.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	if err := status.RefreshCompositionDefinitionStatus(cr, crd, gvr, chartGVK, pkg.PackageURL()); err != nil {
		return reconciler.ExternalObservation{}, fmt.Errorf("error refreshing CompositionDefinition status: %w", err)
	}

	cr.SetConditions(rtv1.Available())

	return reconciler.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: true,
	}, nil
}

// applyHubCompositionCRD ensures the generated composition CRD also exists on the hub (mgmt) for a
// remote CompositionDefinition, so the desired Composition can be authored and validated on the hub
// — the hub-side reflector then mirrors it onto the spoke. For a local CD the CRD already lives on
// the provisioning cluster (which is the hub), so this is a no-op.
//
// A copy of crd is applied so the hub apply never mutates the caller's object; the hub's own
// ApplyOrUpdateCRD adds the composition-version printer column and waits for the CRD to become
// established, independently of the spoke apply. Cross-cluster CRD-version skew (hub vs spoke) is
// tracked in docs/design/remote-composition-mirror.md §7.
func (e *external) applyHubCompositionCRD(ctx context.Context, crd *apiextensionsv1.CustomResourceDefinition) error {
	if !e.remote {
		return nil
	}
	if _, err := crdclient.ApplyOrUpdateCRD(ctx, e.mgmt, e.mgmtDynamic, crd.DeepCopy()); err != nil {
		return fmt.Errorf("error applying composition CRD to hub: %w", err)
	}
	return nil
}

// syncHubCompositionCRDVersions converges the hub (mgmt) composition CRD's served/stored version set
// onto the finalized spoke CRD's, closing the hub↔spoke version skew (docs/design/remote-composition-mirror.md §7).
// The hub apply (applyHubCompositionCRD -> ApplyOrUpdateCRD) only ever APPENDS versions, while the
// spoke's version set is PRUNED over chart-version bumps (pruneStaleServedVersions). Without this the
// hub CRD keeps every version hop's served endpoint forever, drifting from the spoke — dead weight,
// and a stale served endpoint a hub client could pick. No-op for local CDs (the composition CRD lives
// only on the provisioning cluster, which is the hub).
//
// spokeCRD is the LIVE, post-prune spoke CRD whose Spec.Versions is the source of truth; its
// per-version schemas already carry the composition-version printer column, and its Spec.Conversion is
// None — mirroring them preserves both. This never touches the spoke, pruneSet, or Status.Managed: it
// reads the hub CRD, reconciles its Spec.Versions to the spoke's (adds missing, drops versions the
// spoke no longer serves — never "vacuum"), and applies.
func (e *external) syncHubCompositionCRDVersions(ctx context.Context, spokeCRD *apiextensionsv1.CustomResourceDefinition) error {
	if !e.remote || spokeCRD == nil {
		return nil
	}
	gr := schema.GroupResource{Group: spokeCRD.Spec.Group, Resource: spokeCRD.Spec.Names.Plural}
	hubCRD, err := crdclient.Get(ctx, e.mgmt, gr)
	if err != nil {
		return fmt.Errorf("error getting hub composition CRD for version sync: %w", err)
	}
	if hubCRD == nil {
		// applyHubCompositionCRD runs earlier in this reconcile; if the hub CRD is somehow absent
		// there is nothing to converge here.
		return nil
	}

	// Desired served-version set = the spoke's (vacuum + the current served version + any coexisting
	// served version the spoke still keeps).
	want := map[string]bool{}
	for i := range spokeCRD.Spec.Versions {
		want[spokeCRD.Spec.Versions[i].Name] = true
	}
	// Versions the hub carries that the spoke has pruned are stale (never "vacuum", the hub storage
	// version). Also track whether the hub is missing any version the spoke serves.
	pruneSet := map[string]bool{}
	var pruned []string
	have := map[string]bool{}
	for i := range hubCRD.Spec.Versions {
		name := hubCRD.Spec.Versions[i].Name
		have[name] = true
		if name != "vacuum" && !want[name] {
			pruneSet[name] = true
			pruned = append(pruned, name)
		}
	}
	missing := false
	for name := range want {
		if !have[name] {
			missing = true
			break
		}
	}
	if len(pruneSet) == 0 && !missing {
		return nil // hub already matches the spoke's version set
	}

	// The apiserver forbids removing a version from spec.versions while it is still listed in
	// status.storedVersions (a version that was, at some point, the hub's storage version — e.g. the
	// first served version before "vacuum" took over). Trim the pruned versions out of storedVersions
	// FIRST via the status subresource, then rewrite spec.versions — mirroring the spoke prune. Safe:
	// the hub's storage version is "vacuum" (never pruned), so no live data is lost.
	if len(pruneSet) > 0 {
		var keptStored []string
		for _, sv := range hubCRD.Status.StoredVersions {
			if !pruneSet[sv] {
				keptStored = append(keptStored, sv)
			}
		}
		if len(keptStored) != len(hubCRD.Status.StoredVersions) {
			hubCRD.Status.StoredVersions = keptStored
			if err := e.mgmt.Status().Update(ctx, hubCRD); err != nil {
				return fmt.Errorf("trimming hub storedVersions before version sync: %w", err)
			}
		}
	}

	// Mirror the spoke's finalized version set (schemas, served/storage flags, printer columns) onto
	// the hub in one shot: this both drops the pruned versions and adds any the hub was missing.
	hubCRD.Spec.Versions = make([]apiextensionsv1.CustomResourceDefinitionVersion, 0, len(spokeCRD.Spec.Versions))
	for i := range spokeCRD.Spec.Versions {
		hubCRD.Spec.Versions = append(hubCRD.Spec.Versions, *spokeCRD.Spec.Versions[i].DeepCopy())
	}
	// Keep None conversion and the composition-version printer column explicit (idempotent): the spoke
	// versions already carry both, but this guards a spoke CRD that predates either.
	if spokeCRD.Spec.Conversion != nil {
		hubCRD.Spec.Conversion = spokeCRD.Spec.Conversion.DeepCopy()
	}
	crdutils.AddCompositionVersionColumn(hubCRD)

	// A preceding Status().Update (or the direct client used for remote targets) can leave the CRD's
	// TypeMeta unset; kube.Apply derives the GVK from the object, so set it explicitly — mirroring
	// crdclient.Get.
	hubCRD.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
	if err := kube.Apply(ctx, e.mgmtDynamic, apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"), hubCRD, kube.ApplyOptions{}); err != nil {
		return fmt.Errorf("applying hub composition CRD version sync: %w", err)
	}
	e.log.Info("Synced hub composition CRD served versions to spoke", "gvr", gr.String(), "pruned", pruned)
	return nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*compositiondefinitionsv1alpha1.CompositionDefinition)
	if !ok {
		return fmt.Errorf(errNotCR)
	}

	log := e.log.WithValues("operation", "create")
	ctx = contexttools.CtxWithLogger(ctx, log)

	log.Info("Creating CompositionDefinition")

	pkg, dir, err := chart.ChartInfoFromSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return err
	}

	gvk, err := chart.ChartGroupVersionKind(pkg, dir)
	if err != nil {
		return err
	}

	specSchemaBytes, err := chart.ChartJsonSchema(pkg, dir)
	if err != nil {
		return fmt.Errorf("error getting JSON schema: %w", err)
	}
	crd, err := crdutils.GenerateCRD(specSchemaBytes, gvk)
	if err != nil {
		return fmt.Errorf("error generating CRD: %w", err)
	}
	if crd == nil {
		return fmt.Errorf("error generating CRD: crd is nil")
	}
	if err := crdutils.InjectStatusFields(crd, statusFieldsFromSpec(cr)); err != nil {
		return fmt.Errorf("error injecting declared status fields: %w", err)
	}

	gvr, err := crdclient.ApplyOrUpdateCRD(ctx, e.kube, e.dynamic, crd)
	if err != nil {
		return fmt.Errorf("error applying or updating CRD: %w", err)
	}

	// For a remote CD the composition CRD must also exist on the hub, so the desired Composition can
	// be authored/validated there (it lives only on the spoke otherwise). No-op for local CDs.
	if err := e.applyHubCompositionCRD(ctx, crd); err != nil {
		return err
	}

	if err := e.ensureCompositionVersionPolicy(ctx); err != nil {
		return err
	}

	// Seed a remote target BEFORE deploying the cdc: Deploy projects the cdc into cr.Namespace on
	// the spoke, so that namespace must already exist there; and the cdc resolves its package from
	// a local shadow CompositionDefinition. No-op for local deployments.
	if err := e.seedRemoteTargetIfNeeded(ctx, cr, gvr, gvk); err != nil {
		return err
	}

	opts := deploy.DeployOptions{
		RBACFolderPath:         CDCrbacConfigFolder,
		DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
		KubeClient:             e.kube,
		Namespace:              cr.Namespace,
		GVR:                    gvr,
		Spec:                   cr.Spec.Chart.DeepCopy(),
		Controller:             cr.Spec.Controller.DeepCopy(),
		DeploymentTemplatePath: CDCtemplateDeploymentPath,
		ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
		JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
		ServiceTemplatePath:    ServiceTemplatePath,
		JsonSchemaBytes:        specSchemaBytes,
		DynClient:              e.dynamic,
		StatusDataTemplate:     encodeStatusDataTemplate(cr),
		ApiRefName:             apiRefName(cr),
		ApiRefNamespace:        apiRefNamespace(cr),
		ApiRefExtras:           encodeApiRefExtras(cr),
		AuthnNamespace:         AuthnNamespace,
		SnowplowURL:            SnowplowURL,
		AuthnURL:               AuthnURL,
		SelfSAName:             SelfSAName,
		SelfSANamespace:        SelfSANamespace,
		SelfGroup:              SelfGroup,
	}

	dig, err := deploy.Deploy(ctx, e.kube, opts)
	if err != nil {
		return err
	}

	log.Debug("Dynamic Controller successfully deployed",
		"gvr", gvr.String(),
		"namespace", cr.Namespace,
	)

	cr.Status.Digest = dig

	return nil
}

// seedRemoteTargetIfNeeded makes a projected cdc self-sufficient on a remote target. No-op for
// local deployments. When the CompositionDefinition deploys to a remote KubernetesTarget,
// core-provider does not run there, so it seeds what the cdc needs but nothing else installs:
//   - Inc 1: the compositiondefinitions CRD + a status-only shadow CompositionDefinition (the cdc's
//     package getter reads it) + the target namespace.
//   - Inc 2: the chart-inspector workload (SA/RBAC/Deployment/Service), read from the management
//     cluster and projected under the same name/namespace, so the cdc's baked URL_CHART_INSPECTOR
//     resolves locally on the spoke.
func (e *external) seedRemoteTargetIfNeeded(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource, gvk schema.GroupVersionKind) error {
	if !clusterkube.IsRemote(cr.Spec.Deploy) {
		return nil
	}
	pkgFS, err := chartfs.ForSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return fmt.Errorf("loading chart for remote seed: %w", err)
	}

	// C2 — packageURL reachability. The resolved chart URL is passed through to the projected cdc
	// unchanged, so it must be reachable FROM the spoke. A cluster-local URL (a management-cluster
	// Service DNS / localhost / private IP) resolves on the hub but not on the spoke, so the
	// composition would silently fail. Surface it as a condition instead of projecting it.
	if pkgURL := pkgFS.PackageURL(); deploy.IsClusterLocalChartURL(pkgURL) {
		cond := rtv1.Unavailable().WithMessage(fmt.Sprintf(
			"chart %q is cluster-local to the management cluster and unreachable from the remote target; "+
				"publish it to a spoke-reachable registry (public OCI/HTTPS)", pkgURL))
		cond.Reason = "RemoteChartUnreachable"
		cr.SetConditions(cond)
		return fmt.Errorf("remote chart %q is cluster-local and unreachable from the target", pkgURL)
	}

	if err := deploy.SeedRemoteTarget(ctx, e.kube, e.dynamic, deploy.RemoteSeedOptions{
		Namespace:  cr.Namespace,
		CDName:     cr.Name,
		Chart:      cr.Spec.Chart,
		PackageURL: pkgFS.PackageURL(),
		APIVersion: gvr.GroupVersion().String(),
		Kind:       gvk.Kind,
		Resource:   gvr.Resource,
	}); err != nil {
		return fmt.Errorf("seeding remote target: %w", err)
	}

	// Inc 2 — project the chart-inspector so the projected cdc's URL_CHART_INSPECTOR (a management-
	// cluster Service DNS baked into its ConfigMap) resolves on the spoke. Coordinates come from the
	// same cdc-configmap template core-provider uses to wire every cdc, so no new config is needed.
	coords, err := deploy.ChartInspectorCoordsFromConfigmapTemplate(CDCtemplateConfigmapPath)
	if err != nil {
		return fmt.Errorf("resolving chart-inspector coordinates for remote seed: %w", err)
	}
	if err := deploy.ProjectChartInspector(ctx, e.mgmt, e.kube, e.dynamic, coords); err != nil {
		return fmt.Errorf("projecting chart-inspector to remote target: %w", err)
	}
	return nil
}

// teardownRemoteSeedIfNeeded reverses the remote seed when a remote CompositionDefinition is deleted:
// it removes this CD's shadow CompositionDefinition on the spoke and, ref-counted by the shadow CDs
// still present there, the SHARED chart-inspector set and the compositiondefinitions CRD (only when
// this was the last remote composition on the target). No-op for local deployments.
func (e *external) teardownRemoteSeedIfNeeded(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition) error {
	if !clusterkube.IsRemote(cr.Spec.Deploy) {
		return nil
	}
	coords, err := deploy.ChartInspectorCoordsFromConfigmapTemplate(CDCtemplateConfigmapPath)
	if err != nil {
		return fmt.Errorf("resolving chart-inspector coordinates for remote teardown: %w", err)
	}
	if err := deploy.TeardownRemoteSeed(ctx, e.kube, e.dynamic, deploy.RemoteSeedTeardownOptions{
		Namespace:      cr.Namespace,
		CDName:         cr.Name,
		ChartInspector: coords,
	}); err != nil {
		return fmt.Errorf("tearing down remote seed: %w", err)
	}
	return nil
}

func (e *external) Update(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*compositiondefinitionsv1alpha1.CompositionDefinition)
	if !ok {
		return fmt.Errorf(errNotCR)
	}

	log := e.log.WithValues("operation", "update")
	ctx = contexttools.CtxWithLogger(ctx, log)

	log.Info("Updating CompositionDefinition")

	pkg, dir, err := chart.ChartInfoFromSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return fmt.Errorf("error getting chart info: %w", err)
	}
	pkgFS, err := chartfs.ForSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return err
	}

	gvk, err := chart.ChartGroupVersionKind(pkg, dir)
	if err != nil {
		return err
	}

	specSchemaBytes, err := chart.ChartJsonSchema(pkg, dir)
	if err != nil {
		return fmt.Errorf("error getting JSON schema: %w", err)
	}
	crd, err := crdutils.GenerateCRD(specSchemaBytes, gvk)
	if err != nil {
		return fmt.Errorf("error generating CRD: %w", err)
	}
	if crd == nil {
		return fmt.Errorf("error generating CRD: crd is nil")
	}
	if err := crdutils.InjectStatusFields(crd, statusFieldsFromSpec(cr)); err != nil {
		return fmt.Errorf("error injecting declared status fields: %w", err)
	}

	gvr, err := crdclient.ApplyOrUpdateCRD(ctx, e.kube, e.dynamic, crd)
	if err != nil {
		return fmt.Errorf("error applying or updating CRD: %w", err)
	}

	// For a remote CD the composition CRD must also exist on the hub, so the desired Composition can
	// be authored/validated there (it lives only on the spoke otherwise). No-op for local CDs.
	if err := e.applyHubCompositionCRD(ctx, crd); err != nil {
		return err
	}

	if err := e.ensureCompositionVersionPolicy(ctx); err != nil {
		return err
	}

	// Seed the remote target BEFORE deploying the cdc (namespace must exist for the cdc; shadow
	// CompositionDefinition must exist for the cdc's package getter). No-op for local deployments.
	if err := e.seedRemoteTargetIfNeeded(ctx, cr, gvr, gvk); err != nil {
		return err
	}

	opts := deploy.DeployOptions{
		RBACFolderPath:         CDCrbacConfigFolder,
		DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
		KubeClient:             e.kube,
		Namespace:              cr.Namespace,
		GVR:                    gvr,
		Spec:                   cr.Spec.Chart.DeepCopy(),
		Controller:             cr.Spec.Controller.DeepCopy(),
		DeploymentTemplatePath: CDCtemplateDeploymentPath,
		ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
		JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
		ServiceTemplatePath:    ServiceTemplatePath,
		JsonSchemaBytes:        specSchemaBytes,
		DynClient:              e.dynamic,
		StatusDataTemplate:     encodeStatusDataTemplate(cr),
		ApiRefName:             apiRefName(cr),
		ApiRefNamespace:        apiRefNamespace(cr),
		ApiRefExtras:           encodeApiRefExtras(cr),
		AuthnNamespace:         AuthnNamespace,
		SnowplowURL:            SnowplowURL,
		AuthnURL:               AuthnURL,
		SelfSAName:             SelfSAName,
		SelfSANamespace:        SelfSANamespace,
		SelfGroup:              SelfGroup,
	}

	dig, err := deploy.Deploy(ctx, e.kube, opts)
	if err != nil {
		return fmt.Errorf("error deploying dynamic controller: %w", err)
	}

	cr.Status.Digest = dig

	log.Debug("Dynamic Controller successfully updated",
		"gvr", gvr.String(),
		"namespace", cr.Namespace,
	)

	oldGVK := cr.Status.CurrentGVK()
	oldGVR := cr.Status.CurrentGVR()

	// UpgradePolicy (4.1) gates instance migration AND the retirement of the old version's
	// controller together: retiring the old controller without migrating its instances would orphan
	// the coexisting old-version instances. When migration is not approved (Manual without the
	// upgrade-to-version annotation, or Paused) both are skipped — the new version is still served
	// (deployed above), so old and new coexist, and served-version pruning keeps any version whose
	// label an instance still carries. Automatic (default) preserves the eager self-healing behavior.
	migrate := cr.MigrationApproved(gvk.Version)
	if !migrate {
		log.Debug("Skipping instance migration and old-controller retirement per UpgradePolicy; old version coexists",
			"upgradePolicy", cr.Spec.UpgradePolicy, "current", gvk.Version)
	}
	// Undeploy olders versions of the CRD
	if migrate && oldGVK != gvk {
		for _, vi := range cr.Status.Managed.VersionInfo {
			if oldGVK.Kind == cr.Status.Managed.Kind && oldGVK.Version == vi.Version {
				// Reference-counted retirement: one CRD/Kind can be shared by multiple
				// CompositionDefinitions at different versions, and a per-(CRD,version)
				// controller is shared across them. Only retire this version's controller when
				// NO OTHER definition still targets it — otherwise we'd tear a controller out
				// from under another definition's instances.
				referenced, refErr := e.versionReferencedByAnotherDefinition(ctx, schema.GroupVersionKind{
					Group: oldGVK.Group, Kind: oldGVK.Kind, Version: vi.Version,
				}, cr.Name, cr.Namespace)
				if refErr != nil {
					return fmt.Errorf("error checking references for version %s: %w", vi.Version, refErr)
				}
				if referenced {
					log.Debug("Skipping controller retirement: version still referenced by another CompositionDefinition", "version", vi.Version)
					continue
				}
				err = deploy.Undeploy(ctx, e.kube, deploy.UndeployOptions{
					DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
					RBACFolderPath:         CDCrbacConfigFolder,
					DeploymentTemplatePath: CDCtemplateDeploymentPath,
					ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
					JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
					ServiceTemplatePath:    ServiceTemplatePath,
					DynamicClient:          e.dynamic,
					Spec:                   (*compositiondefinitionsv1alpha1.ChartInfo)(vi.Chart),
					GVR:                    oldGVR,
					KubeClient:             e.kube,
					Namespace:              cr.Namespace,
					SkipCRD:                true,
					AuthnNamespace:         AuthnNamespace,
				})
				if err != nil {
					return fmt.Errorf("error undeploying older version of dynamic controller: %w", err)
				}
				log.Debug("Undeployed older versions of dynamic controller", "gvr", oldGVR.String())
			}
		}
	}
	// Migrate any composition still carrying a previous served version's label onto the
	// current version. Driven by the managed served-versions list rather than a single
	// status.apiVersion comparison so it is idempotent and self-healing: stragglers from a
	// prior transition — or a composition (re)written through an old endpoint after status
	// advanced — are migrated on a later reconcile instead of being orphaned forever (the
	// new label-scoped controller never selects an old-labelled CR). UpdateCompositionsVersion
	// is a no-op when nothing carries the old label, so this is safe to run every transition.
	if migrate {
		log.Debug("Migrating Compositions to current version", "gvr", gvr.String())
		owner := getters.DefinitionRef{Name: cr.Name, Namespace: cr.Namespace}
		for _, vi := range cr.Status.Managed.VersionInfo {
			if vi.Version == gvk.Version {
				continue
			}
			// List + re-stamp THROUGH the current served endpoint (gvr, whose version is
			// gvk.Version): the composition-version policy stamps the request's served version,
			// so writing via the current endpoint makes it agree with the relabel instead of
			// re-stamping the old version. Scoped to THIS definition so a shared CRD's other
			// definitions (legitimately on an older version) are never touched.
			if err := getters.UpdateCompositionsVersion(ctx, e.dynamic, gvr, vi.Version, gvk.Version, owner); err != nil {
				return fmt.Errorf("error migrating compositions from version %s: %w", vi.Version, err)
			}
		}
	}

	// Prune served versions that are no longer in use. After migration every composition is on the
	// current version, so any OTHER non-vacuum served version that no definition references and no
	// instance carries its label is dead weight in the CRD's spec.versions (#103) — and a stale
	// served endpoint a client can pick that silently prunes fields written through it. Best-effort:
	// a failure is logged and retried on the next reconcile; it never blocks the reconcile.
	if err := e.pruneStaleServedVersions(ctx, gvk, gvr, cr.Name, cr.Namespace); err != nil {
		log.Info("served-version prune failed (Observe will re-drive)", "error", err.Error())
	}

	// Refresh status against the LIVE CRD (post-deploy/prune), not the freshly generated single-version
	// `crd`: the VersionInfo projection keeps only versions present in the CRD it is given, so feeding
	// it the generated CRD would collapse VersionInfo to just the current version every reconcile. The
	// live CRD carries the full served-version set, so the projection stays stable and per-version
	// Chart is preserved. Fall back to the generated CRD if the live one can't be fetched.
	liveCRD, getErr := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	// Converge the hub composition CRD's served-version set onto the finalized spoke CRD, so the hub
	// does not accumulate served versions the spoke has pruned (hub↔spoke version skew). Uses the LIVE
	// spoke CRD (post-prune) as the source of truth; skipped when it can't be read (nothing reliable to
	// mirror). Best-effort, like the spoke prune above: a failure is logged and re-driven on the next
	// version bump rather than blocking the reconcile. No-op for local CDs.
	if getErr == nil && liveCRD != nil {
		if err := e.syncHubCompositionCRDVersions(ctx, liveCRD); err != nil {
			log.Info("hub composition CRD version sync failed (next version bump will re-drive)", "error", err.Error())
		}
	}
	if getErr != nil || liveCRD == nil {
		liveCRD = crd
	}
	if err := status.RefreshCompositionDefinitionStatus(cr, liveCRD, gvr, gvk, pkgFS.PackageURL()); err != nil {
		return fmt.Errorf("error refreshing CompositionDefinition status: %w", err)
	}

	return nil
}

// statusGVR returns the generated GroupVersionResource recorded on the CompositionDefinition status
// at deploy time (apiVersion + resource). For a remote target this is the source of truth, since the
// generated CRD is not visible to the management-cluster pluralizer. ok is false until status records it.
func statusGVR(cr *compositiondefinitionsv1alpha1.CompositionDefinition) (schema.GroupVersionResource, bool) {
	if cr.Status.ApiVersion == "" || cr.Status.Resource == "" {
		return schema.GroupVersionResource{}, false
	}
	gv, err := schema.ParseGroupVersion(cr.Status.ApiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false
	}
	return gv.WithResource(cr.Status.Resource), true
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*compositiondefinitionsv1alpha1.CompositionDefinition)
	if !ok {
		return fmt.Errorf(errNotCR)
	}
	log := e.log.WithValues("operation", "delete")
	ctx = contexttools.CtxWithLogger(ctx, log)

	cr.SetConditions(rtv1.Deleting())

	pkg, dir, err := chart.ChartInfoFromSpec(ctx, e.mgmtDynamic, cr.Spec.Chart)
	if err != nil {
		return fmt.Errorf("error getting chart info: %w", err)
	}

	gvk, err := chart.ChartGroupVersionKind(pkg, dir)
	if err != nil {
		return fmt.Errorf("error getting chart GVK: %w", err)
	}

	var gvr schema.GroupVersionResource
	crdExist := true
	gvr, err = e.pluralizer.GVKtoGVR(gvk)
	if apierrors.IsNotFound(err) {
		// The pluralizer resolves against the MANAGEMENT cluster, but a remote target's generated CRD
		// lives on the SPOKE — so discovery misses it and we would skip the whole Undeploy /
		// composition-deletion block, orphaning the spoke's cdc, generated CRD and composition
		// instances. Fall back to the GVR recorded on status at deploy time so a remote delete still
		// tears the spoke down. Local deploys keep the discovery result (status may be empty).
		if sgvr, ok := statusGVR(cr); ok {
			gvr, crdExist, err = sgvr, true, nil
			log.Debug("Plural not found on management cluster; using status GVR for remote teardown", "gvr", gvr.String())
		} else {
			crdExist = false
			log.Debug("Plural not found, CRD not found, skipping deletion", "gvk", gvk.String())
		}
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("error converting GVK to GVR: %w - GVK: %s", err, gvk.String())
	}
	if crdExist {
		lst, err := getters.GetCompositionDefinitionsWithVersion(ctx, e.mgmt, schema.GroupVersionKind{
			Group:   gvk.Group,
			Kind:    gvk.Kind,
			Version: gvk.Version,
		})
		if err != nil {
			return fmt.Errorf("error getting CompositionDefinitions: %w", err)
		}
		// A remote target's generated CRD and cdc are DEDICATED to this CompositionDefinition (each
		// remote CD gets its own projected CRD/cdc on the spoke), so its compositions must always be
		// removed and its CRD always deleted. The len==1 / skipCRD gates below exist for a LOCAL
		// shared CRD (multiple definitions on one version) and are additionally unreliable for a
		// remote CD whose status GVK the management-cluster counting may not see during termination —
		// which left the spoke's instance and generated CRD orphaned. Force the cleanup when remote.
		remote := clusterkube.IsRemote(cr.Spec.Deploy)
		if remote || len(lst) == 1 {
			log.Debug("Deleting Compositions of this version", "gvk", gvk.String(), "remote", remote)

			// Delete compositions of this version manually
			ul, err := getters.GetCompositions(ctx, e.dynamic, gvr)
			if err != nil {
				return fmt.Errorf("error getting compositions: %w", err)
			}

			for i := range ul.Items {
				log.Debug("Deleting composition", "name", ul.Items[i].GetName(), "namespace", ul.Items[i].GetNamespace())
				err := kube.Uninstall(ctx, e.dynamic, gvr, &ul.Items[i], kube.UninstallOptions{})
				if err != nil {
					return err
				}
			}

			ul, err = getters.GetCompositions(ctx, e.dynamic, gvr)
			if err != nil {
				return fmt.Errorf("error getting compositions: %w", err)
			}
			if len(ul.Items) > 0 {
				// Retry until the cdc (still running — Undeploy runs only after this passes) finalizes
				// the instances, so a composition is never orphaned by removing its cdc first.
				return fmt.Errorf("error undeploying CompositionDefinition: waiting for composition deletion")
			}
		}

		var skipCRD bool
		if !remote {
			lst, err = getters.GetCompositionDefinitions(ctx, e.mgmt, schema.GroupKind{
				Group: gvk.Group,
				Kind:  gvk.Kind,
			})
			if err != nil {
				return fmt.Errorf("error getting CompositionDefinitions: %w", err)
			}
			skipCRD = len(lst) > 1
		}
		if skipCRD {
			log.Debug("Skipping CRD deletion, other CompositionDefinitions exist", "gvk", gvk.String())
		} else {
			log.Debug("Deleting CRD", "gvk", gvk.String(), "remote", remote)
		}

		// Retire any coexisting OLD-version controllers first (Manual/Paused UpgradePolicy can leave
		// more than one version's controller running). Their per-version-named Deployment/RBAC/Service
		// would otherwise be orphaned once the CRD is removed below. Best-effort + SkipCRD (the CRD is
		// torn down by the main Undeploy). All composition instances were already deleted above via the
		// current served endpoint (None conversion serves every version's objects), so this only sheds
		// the extra controllers.
		for _, vi := range cr.Status.Managed.VersionInfo {
			if vi.Version == gvr.Version {
				continue
			}
			// The vacuum storage placeholder (and any version never realized as the current chart) has
			// no Chart recorded and thus no controller to retire; Undeploy would dereference the nil
			// Spec (deploy.go) and panic. Only versions that were once served have a Chart.
			if vi.Chart == nil {
				continue
			}
			oldVerGVR := schema.GroupVersionResource{Group: gvr.Group, Version: vi.Version, Resource: gvr.Resource}
			if uerr := deploy.Undeploy(ctx, e.kube, deploy.UndeployOptions{
				DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
				Spec:                   (*compositiondefinitionsv1alpha1.ChartInfo)(vi.Chart),
				KubeClient:             e.kube,
				GVR:                    oldVerGVR,
				Namespace:              cr.Namespace,
				SkipCRD:                true,
				DynamicClient:          e.dynamic,
				RBACFolderPath:         CDCrbacConfigFolder,
				DeploymentTemplatePath: CDCtemplateDeploymentPath,
				ServiceTemplatePath:    ServiceTemplatePath,
				ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
				JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
				AuthnNamespace:         AuthnNamespace,
			}); uerr != nil && !errors.Is(uerr, deploy.ErrCompositionStillExist) {
				log.Debug("Best-effort retire of coexisting old-version controller on delete failed", "version", vi.Version, "error", uerr.Error())
			}
		}

		opts := deploy.UndeployOptions{
			DiscoveryClient:        memory.NewMemCacheClient(e.client.Discovery()),
			Spec:                   cr.Spec.Chart.DeepCopy(),
			KubeClient:             e.kube,
			GVR:                    gvr,
			Namespace:              cr.Namespace,
			SkipCRD:                skipCRD,
			DynamicClient:          e.dynamic,
			RBACFolderPath:         CDCrbacConfigFolder,
			DeploymentTemplatePath: CDCtemplateDeploymentPath,
			ServiceTemplatePath:    ServiceTemplatePath,
			ConfigmapTemplatePath:  CDCtemplateConfigmapPath,
			JsonSchemaTemplatePath: JSONSchemaTemplateConfigmapPath,
			AuthnNamespace:         AuthnNamespace,
		}

		err = deploy.Undeploy(ctx, e.kube, opts)
		if err != nil {
			if errors.Is(err, deploy.ErrCompositionStillExist) {
				return fmt.Errorf("error undeploying CompositionDefinition: waiting for composition deletion")
			}
			return fmt.Errorf("error undeploying CompositionDefinition: %w", err)

		}
	} else {
		log.Debug("CRD not found, deletion has already been completed", "gvk", gvk.String())
	}

	// Remote teardown: clean the spoke-side seed (shadow CD + ref-counted chart-inspector / CD CRD).
	// No-op for local deployments. Runs after Undeploy so the cdc is gone first.
	if err := e.teardownRemoteSeedIfNeeded(ctx, cr); err != nil {
		return err
	}

	return nil
}
