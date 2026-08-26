package composition

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/metrics"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/rbacgen"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/processor"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/tracer"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
	"github.com/krateo-platformops/plumbing/env"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/helm/utils"
	helmutils "github.com/krateo-platformops/plumbing/helm/utils"
	"github.com/krateo-platformops/plumbing/helm/v3"

	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/maps"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/meta"
	"github.com/krateo-platformops/unstructured-runtime/pkg/telemetry"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/statusprojection"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	krateoNamespace = env.String(krateoNamespaceEnvVar, krateoNamespaceDefault)
	helmMaxHistory  = env.Int(helmMaxHistoryEnvvar, 3)
	// pendingOperationGrace: a helm release found in a pending-* status is rolled back ONLY if it
	// has been pending LONGER than this — i.e. genuinely stuck (the controller died mid-operation),
	// not legitimately in-flight. A large composition's upgrade (hundreds of children + hooks) can
	// stay pending for tens of seconds; rolling that back mid-flight caused an Upgrade<->Rollback
	// thrash on the 319-resource portal composition. Default 5m (>= helm's default op timeout), so
	// only operations that exceeded a real upgrade are treated as stuck. Tunable per deployment.
	pendingOperationGrace = env.Duration(pendingGraceEnvvar, 5*time.Minute)
)

const (
	reasonReconciliationGracefullyPaused event.Reason = "ReconciliationGracefullyPaused"

	// Ready-condition reason used while a helm operation is still in flight and this reconcile is
	// deliberately standing down rather than starting a concurrent one.
	reasonPendingHelmOperation = "PendingHelmOperation"

	// Event reasons
	reasonCreated = "CompositionCreated"
	reasonDeleted = "CompositionDeleted"
	reasonUpdated = "CompositionUpdated"

	// Environment variables
	helmMaxHistoryEnvvar  = "HELM_MAX_HISTORY"
	krateoNamespaceEnvVar = "KRATEO_NAMESPACE"
	pendingGraceEnvvar    = "COMPOSITION_CONTROLLER_PENDING_GRACE"

	// Default namespace for Krateo Installation
	krateoNamespaceDefault = "krateo-system"
)

var _ controller.ExternalClient = (*handler)(nil)

type HandlerOptions struct {
	Kubeconfig        *rest.Config
	PackageInfoGetter archive.Getter
	EventRecorder     event.APIRecorder
	Pluralizer        pluralizer.PluralizerInterface
	ChartInspectorUrl string
	SaName            string
	SaNamespace       string
	SafeReleaseName   bool
	Mapper            apimeta.RESTMapper
	// StatusDataTemplate are the declarative status projections (snowplow
	// widgetDataTemplate shape) shipped by core-provider from the CompositionDefinition.
	StatusDataTemplate []statusprojection.Mapping
	// APIResolver, when set, resolves the CompositionDefinition's apiRef each reconcile to the
	// keyed ".api" projection source. nil disables apiRef resolution (the ".api" source is
	// simply absent and api-dependent mappings degrade individually).
	APIResolver APIResolver
}

// APIResolver resolves the apiRef's RESTAction (via snowplow, under the CDC's authn identity)
// to the keyed `.api.<callName>` source map for a specific composition instance.
type APIResolver interface {
	Resolve(ctx context.Context, mg *unstructured.Unstructured) (map[string]any, error)
}

func NewHandler(opts *HandlerOptions) controller.ExternalClient {
	return &handler{
		kubeconfig:         opts.Kubeconfig,
		pluralizer:         opts.Pluralizer,
		packageInfoGetter:  opts.PackageInfoGetter,
		eventRecorder:      opts.EventRecorder,
		chartInspectorUrl:  opts.ChartInspectorUrl,
		saName:             opts.SaName,
		saNamespace:        opts.SaNamespace,
		safeReleaseName:    opts.SafeReleaseName,
		mapper:             opts.Mapper,
		statusDataTemplate: opts.StatusDataTemplate,
		apiResolver:        opts.APIResolver,
	}
}

type handler struct {
	kubeconfig    *rest.Config
	pluralizer    pluralizer.PluralizerInterface
	eventRecorder event.APIRecorder
	mapper        apimeta.RESTMapper

	packageInfoGetter archive.Getter

	chartInspectorUrl string
	saName            string
	saNamespace       string
	// Feature flag to disable random suffix in Helm release names. This is highly discouraged as it can lead to release name collisions, but it can be useful for certain complex charts that have issues with long release names.
	safeReleaseName bool
	// statusDataTemplate are the declarative ${ jq } status projections from the
	// CompositionDefinition, evaluated each reconcile and written under .status.
	statusDataTemplate []statusprojection.Mapping
	// apiResolver resolves the apiRef to the ".api" projection source each reconcile; nil
	// when no apiRef is declared.
	apiResolver APIResolver
}

func (h *handler) Observe(ctx context.Context, mg *unstructured.Unstructured) (controller.ExternalObservation, error) {
	mg = mg.DeepCopy()

	log := xcontext.Logger(ctx)

	log = log.WithValues("op", "Observe").
		WithValues("apiVersion", mg.GetAPIVersion()).
		WithValues("kind", mg.GetKind()).
		WithValues("name", mg.GetName()).
		WithValues("namespace", mg.GetNamespace())

	// If the Composition is being deleted, do NOT run the drift observation. Observe computes
	// drift via a helm Upgrade, which fails with "has no deployed releases" once the release is
	// mid-uninstall — and resync re-enqueues Observe every cycle, so the reconcile loops in
	// ReconcileError and never finalizes. Report the resource as existing + up-to-date so the
	// runtime routes the deletion to the Delete handler, which performs/completes the uninstall
	// (it tolerates an already-removed release) and clears the finalizer.
	if mg.GetDeletionTimestamp() != nil {
		log.Debug("Composition has a deletionTimestamp; skipping drift observe — deletion is handled by Delete.")
		return controller.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: true,
		}, nil
	}

	dyn, err := dynamic.NewForConfig(h.kubeconfig)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("creating dynamic client: %w", err)
	}

	updateOpts := tools.UpdateOptions{
		Pluralizer:    h.pluralizer,
		DynamicClient: dyn,
	}

	compositionMeta.SetReleaseName(mg, compositionMeta.CalculateReleaseName(mg, h.safeReleaseName))
	releaseName := compositionMeta.GetReleaseName(mg)
	_, paused := compositionMeta.GetGracefullyPausedTime(mg)
	if paused && compositionMeta.IsGracefullyPaused(mg) {
		log.Debug("Composition is gracefully paused, skipping observe.")
		h.eventRecorder.Event(mg, event.Normal(reasonReconciliationGracefullyPaused, "Observe", "Reconciliation is paused via the gracefully paused annotation."))
		return controller.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: true,
		}, nil
	}
	// Observe MUST be a pure read of external state. It previously persisted metadata here (release
	// name + CD def-ref labels via tools.Update), which bumped the CR's resourceVersion mid-observe.
	// A concurrent writer (an umbrella re-applying the instance, a human kubectl edit, or the
	// reconcile's own external-create annotation write) then made that write 409, so Observe returned
	// an error — and the runtime's incomplete-create recovery treats an Observe error as "cannot
	// determine creation result" and refuses, wedging the resource forever. Metadata is now persisted
	// only in the mutating phases (Create/Update). The ONLY residual write is clearing a STALE
	// gracefully-paused-time annotation (present but not actually paused), and only when present — so
	// the steady-state Observe writes nothing.
	if paused {
		meta.RemoveAnnotations(mg, compositionMeta.AnnotationKeyReconciliationGracefullyPausedTime)
		if mg, err = tools.Update(ctx, mg, updateOpts); err != nil {
			return controller.ExternalObservation{}, fmt.Errorf("clearing stale paused annotation: %w", err)
		}
	}

	if h.packageInfoGetter == nil {
		return controller.ExternalObservation{}, fmt.Errorf("helm chart package info getter must be specified")
	}
	// pkg is resolved (and used below for the drift dry-run render); the CD def-ref labels it yields
	// are stamped onto the instance in Create/Update, not here — Observe stays side-effect-free.
	pkg, err := h.packageInfoGetter.WithLogger(log).Get(mg)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("getting package info: %w", err)
	}

	hc, err := helm.NewClient(h.kubeconfig,
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
		helm.WithCache(),
	)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("creating helm client: %w", err)
	}

	rel, err := hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("finding helm release: %w", err)
	}
	if rel == nil {
		log.Debug("Release not found.")
		return controller.ExternalObservation{
			ResourceExists:   false,
			ResourceUpToDate: false,
		}, nil
	}

	if isIncompleteHelmOperation(rel.Status) {
		// A pending-* OR uninstalling status means a helm operation is in flight OR its process died
		// mid-flight — helm labels both the same and never re-labels a crash. (StatusUninstalling is
		// included so a release stuck mid-uninstall — e.g. a Delete that died after helm began the
		// uninstall — is detected as stuck and cleared; otherwise it wedges forever, since helm refuses
		// to operate on a release locked in `uninstalling`.) Status alone can't tell in-flight from
		// crashed apart, so we use how long it has been pending (rel.Updated = helm Info.LastDeployed).
		// A LARGE composition's upgrade (hundreds of children + hooks) legitimately stays pending for
		// tens of seconds; the old unconditional rollback reverted such in-flight operations mid-flight
		// (even rolling back a pending-rollback), which — with the reconcile re-enqueue cadence —
		// produced an Upgrade<->Rollback thrash on the 319-resource portal composition.
		pendingFor := time.Since(rel.Updated)
		if pendingFor < pendingOperationGrace {
			// Recent => legitimately in flight. Do NOT roll it back and do NOT start a concurrent
			// operation; report up-to-date so the in-flight op settles and the next reconcile proceeds.
			log.Debug("Release operation in progress; waiting for it to settle.",
				"status", string(rel.Status), "pendingFor", pendingFor.String())

			// Say so on the CR. Returning UpToDate is right — it is what prevents a second concurrent
			// helm operation — but leaving the conditions untouched makes the resource assert something
			// false for up to the whole grace period: a composition whose spec change is sitting
			// unapplied behind the pending lock kept reporting Ready=True "Composition is up-to-date"
			// (reproduced on kind: the rendered child still held the OLD value for ~4 minutes while the
			// CR claimed convergence). Anything gating on Ready then proceeds on a false premise. The
			// inverse is just as wrong: a stale ReconcileError from an earlier attempt would persist
			// here unchanged. Report the wait explicitly instead; the next reconcile past the grace
			// period rolls the lock back and the normal conditions take over.
			pendingCond := condition.FailWithReason(reasonPendingHelmOperation)
			pendingCond.Message = fmt.Sprintf(
				"waiting for an in-flight helm operation to settle (status %s, pending for %s)",
				string(rel.Status), pendingFor.Truncate(time.Second))
			unstructuredtools.SetConditions(mg, pendingCond)
			if _, uerr := updateStatusWithRetry(ctx, mg, updateOpts); uerr != nil {
				// Non-fatal: the wait itself is not an error, and failing the reconcile here would
				// burn retry budget on a purely cosmetic write.
				log.Debug("Could not record the pending-operation condition", "error", uerr)
			}
			return controller.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
		}
		// Stuck longer than any real operation => genuinely wedged (e.g. controller died mid-op). Roll
		// back once to clear the stale pending/uninstalling lock so a fresh upgrade (or a clean re-drive
		// of the uninstall) can proceed — helm refuses to operate on a release stuck pending/uninstalling.
		log.Debug("Composition stuck in an incomplete helm operation past the grace period; rolling back to clear it.",
			"status", string(rel.Status), "pendingFor", pendingFor.String(), "grace", pendingOperationGrace.String())
		rel, err = hc.Rollback(ctx, releaseName, &helmconfig.RollbackConfig{
			MaxHistory:     helmMaxHistory,
			ReleaseVersion: rel.Revision,
		})
		if err != nil {
			return controller.ExternalObservation{}, fmt.Errorf("rolling back release: %w", err)
		}
	}

	compositionGVR, err := h.pluralizer.GVKtoGVR(mg.GroupVersionKind())
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("converting GVK to GVR: %w", err)
	}

	chartInspector := chartinspector.NewChartInspector(h.chartInspectorUrl)
	rbgen := metrics.WrapRBACGen(rbacgen.NewRBACGen(h.saName, h.saNamespace, &chartInspector))
	// Get Resources and generate RBAC
	generated, err := rbgen.
		WithBaseName(releaseName).
		Generate(ctx, rbacgen.Parameters{
			CompositionName:                mg.GetName(),
			CompositionNamespace:           mg.GetNamespace(),
			CompositionGVR:                 compositionGVR,
			CompositionDefinitionName:      pkg.CompositionDefinitionInfo.Name,
			CompositionDefinitionNamespace: pkg.CompositionDefinitionInfo.Namespace,
			CompositionDefintionGVR:        pkg.CompositionDefinitionInfo.GVR,
		})
	if err != nil {
		retErr := fmt.Errorf("generating RBAC using chart-inspector: %w", err)
		condition := condition.Unavailable()
		condition.Message = retErr.Error()
		unstructuredtools.SetConditions(mg, condition)
		_, err = updateStatusWithRetry(ctx, mg, updateOpts)
		if err != nil {
			return controller.ExternalObservation{}, fmt.Errorf("updating status after failure: %w", err)
		}
		return controller.ExternalObservation{}, fmt.Errorf("generating RBAC using chart-inspector: %w", retErr)
	}
	rbInstaller := rbac.NewRBACInstaller(dyn)
	helmMetrics := metrics.NewHelmMetrics(ctx)
	err = helmMetrics.TimedRBAC(func() error {
		return rbInstaller.ApplyRBAC(generated)
	})
	if err != nil {
		retErr := fmt.Errorf("applying rbac: %w", err)
		condition := condition.Unavailable()
		condition.Message = retErr.Error()
		unstructuredtools.SetConditions(mg, condition)
		_, err = updateStatusWithRetry(ctx, mg, updateOpts)
		if err != nil {
			return controller.ExternalObservation{}, fmt.Errorf("updating status after failure: %w", err)
		}
		return controller.ExternalObservation{}, retErr
	}

	tracer := tracer.NewTracer(ctx, meta.IsVerbose(mg))
	cfg := rest.CopyConfig(h.kubeconfig)
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return tracer.WithRoundTripper(rt)
	}
	hc, err = helm.NewClient(cfg,
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithCache(),
	)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("getting helm client: %w", err)
	}

	values, err := helmutils.ValuesFromSpec(mg)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("getting spec values: %w", err)
	}
	err = values.InjectGlobalValues(mg, h.pluralizer, krateoNamespace)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("injecting global values: %w", err)
	}
	postrenderLabels, err := utils.LabelPostRenderFromSpec(mg, h.pluralizer, krateoNamespace)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("creating label post renderer: %w", err)
	}
	// Stamp the active reconcile trace onto every child manifest (krateo.io/traceparent) so the
	// child composition's controller continues the same distributed trace. No-op when tracing is
	// off; excluded from the release digest (processor.ComputeReleaseDigest) so it never churns.
	tpCarrier := map[string]string{}
	telemetry.InjectTraceparent(ctx, tpCarrier)
	postrenderLabels.WithTraceparent(tpCarrier[meta.AnnotationKeyTraceparent], tpCarrier[meta.AnnotationKeyTracestate])
	// Self-healing "apply-if-changed" reconcile. Observe is no longer read-only nor an
	// unconditional live Upgrade (the old behaviour created an identical revision every ~60s cycle
	// even at steady state — infinite revision churn and needless API-server load, upstream
	// composition-dynamic-controller#184). Reconcile runs helm's 3-way merge (KubeClient.Update)
	// through the EXPORTED helm API every cycle: it recreates children deleted out-of-band and
	// patches drifted fields, converging the cluster — but only writes a new helm revision + runs
	// hooks when the live cluster was ACTUALLY mutated (a create, delete, or non-empty patch,
	// detected via resourceVersion bumps rather than helm's over-eager Result.Updated). At steady
	// state it is a no-op: no revision, no hooks. ResourceUpToDate reflects whether the cluster
	// already matched the desired state; the digest below is computed for STATUS REPORTING only,
	// not as an up-to-date gate.
	reconcileRes, err := hc.Reconcile(ctx, releaseName, pkg.URL, &helmconfig.UpgradeConfig{
		ActionConfig: &helmconfig.ActionConfig{
			ChartVersion:          pkg.Version,
			ChartName:             pkg.Repo,
			Username:              pkg.Auth.Username,
			Password:              pkg.Auth.Password,
			InsecureSkipTLSverify: pkg.InsecureSkipTLSverify,
			Values:                values,
			PostRenderer:          postrenderLabels,
			// Adopt an existing child object rather than aborting the whole release when it carries
			// non-Helm ownership metadata (e.g. a composition instance created/edited out-of-band).
			// Without this, one un-adoptable child 500s the entire reconcile ("cannot be imported
			// into the current release: invalid ownership metadata") and wedges the platform (D1,
			// 2026-07-08); with it the release takes ownership, self-healing the conflict.
			TakeOwnership: true,
		},
		MaxHistory: helmMaxHistory,
	})
	if err != nil {
		retErr := fmt.Errorf("reconciling helm chart: %w", err)
		condition := condition.Unavailable()
		condition.Message = retErr.Error()
		unstructuredtools.SetConditions(mg, condition)
		_, err = updateStatusWithRetry(ctx, mg, updateOpts)
		if err != nil {
			return controller.ExternalObservation{}, fmt.Errorf("updating status after failure: %w", err)
		}
		return controller.ExternalObservation{}, retErr
	}

	// The reconcile converged the cluster in-place. When it detected a real change it also wrote a
	// fresh revision (reconcileRes.Release); otherwise Release is the stored release. Report the
	// current release's digest under status.digest for observability.
	upgradedRel := reconcileRes.Release
	digest, err := processor.ComputeReleaseDigest(upgradedRel)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("computing release digest: %w", err)
	}

	previousDigest, err := maps.NestedString(mg.Object, "status", "digest")
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("getting previous digest from status: %w", err)
	}

	if reconcileRes.Changed {
		log.Debug("Composition drift detected and self-healed.", "package", pkg.URL,
			"created", reconcileRes.Created, "deleted", reconcileRes.Deleted, "patched", reconcileRes.PatchedUpdated)
		return controller.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	err = h.setStatus(ctx, mg, &statusManagerOpts{
		force:           false,
		resources:       nil, // we don't need to set resources here as they are already set when a resource is created/updated
		previousDigest:  previousDigest,
		digest:          digest,
		message:         "Composition is up-to-date",
		chartURL:        pkg.URL,
		chartVersion:    pkg.Version,
		releaseStatus:   string(rel.Status),
		releaseRevision: rel.Revision,
		releaseName:     rel.Name,
		conditionType:   ConditionTypeAvailable,
	})
	if err != nil {
		return controller.ExternalObservation{}, err
	}

	_, err = updateStatusWithRetry(ctx, mg, updateOpts)
	if err != nil {
		return controller.ExternalObservation{}, err
	}

	log.Debug("Composition Observed - installed", "package", pkg.URL)

	return controller.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: true,
	}, nil
}

func (h *handler) Create(ctx context.Context, mg *unstructured.Unstructured) error {
	mg = mg.DeepCopy()

	log := xcontext.Logger(ctx)
	log = log.WithValues("op", "Create").
		WithValues("apiVersion", mg.GetAPIVersion()).
		WithValues("kind", mg.GetKind()).
		WithValues("name", mg.GetName()).
		WithValues("namespace", mg.GetNamespace())

	dyn, err := dynamic.NewForConfig(h.kubeconfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	updateOpts := tools.UpdateOptions{
		Pluralizer:    h.pluralizer,
		DynamicClient: dyn,
	}

	if _, p := compositionMeta.GetGracefullyPausedTime(mg); p && compositionMeta.IsGracefullyPaused(mg) {
		log.Debug("Composition is gracefully paused, skipping create.")
		h.eventRecorder.Event(mg, event.Normal(reasonReconciliationGracefullyPaused, "Update", "Reconciliation is paused via the gracefully paused annotation."))
		return nil
	}

	compositionMeta.SetReleaseName(mg, compositionMeta.CalculateReleaseName(mg, h.safeReleaseName))
	releaseName := compositionMeta.GetReleaseName(mg)
	mg, err = tools.Update(ctx, mg, updateOpts)
	if err != nil {
		return fmt.Errorf("updating cr with values: %w", err)
	}

	if h.packageInfoGetter == nil {
		return fmt.Errorf("helm chart package info getter must be specified")
	}

	pkg, err := h.packageInfoGetter.WithLogger(log).Get(mg)
	if err != nil {
		return fmt.Errorf("getting package info: %w", err)
	}
	// Stamp the owning CompositionDefinition identity (name/namespace/GVR) onto the instance. Observe
	// used to do this on every reconcile, but Observe is now a pure read; the archive getter reads
	// these def-ref labels to disambiguate the owning CD across chart-version bumps, so they are
	// persisted here in the create (mutating) phase instead.
	compositionMeta.SetCompositionDefinitionLabels(mg, compositionMeta.CompositionDefinitionInfo{
		Name:      pkg.CompositionDefinitionInfo.Name,
		Namespace: pkg.CompositionDefinitionInfo.Namespace,
		GVR:       pkg.CompositionDefinitionInfo.GVR,
	})
	if mg, err = tools.Update(ctx, mg, updateOpts); err != nil {
		return fmt.Errorf("stamping composition-definition labels: %w", err)
	}
	compositionGVR, err := h.pluralizer.GVKtoGVR(mg.GroupVersionKind())
	if err != nil {
		return fmt.Errorf("converting GVK to GVR: %w", err)
	}

	chartInspector := chartinspector.NewChartInspector(h.chartInspectorUrl)
	rbgen := metrics.WrapRBACGen(rbacgen.NewRBACGen(h.saName, h.saNamespace, &chartInspector))
	// Get Resources and generate RBAC
	generated, err := rbgen.
		WithBaseName(releaseName).
		Generate(ctx, rbacgen.Parameters{
			CompositionName:                mg.GetName(),
			CompositionNamespace:           mg.GetNamespace(),
			CompositionGVR:                 compositionGVR,
			CompositionDefinitionName:      pkg.CompositionDefinitionInfo.Name,
			CompositionDefinitionNamespace: pkg.CompositionDefinitionInfo.Namespace,
			CompositionDefintionGVR:        pkg.CompositionDefinitionInfo.GVR,
		})
	if err != nil {
		return fmt.Errorf("generating RBAC using chart-inspector: %w", err)
	}
	rbInstaller := rbac.NewRBACInstaller(dyn)
	helmMetrics := metrics.NewHelmMetrics(ctx)
	err = helmMetrics.TimedRBAC(func() error {
		return rbInstaller.ApplyRBAC(generated)
	})
	if err != nil {
		return fmt.Errorf("installing rbac: %w", err)
	}

	hc, err := helm.NewClient(h.kubeconfig,
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
	)
	if err != nil {
		return fmt.Errorf("creating helm client: %w", err)
	}

	values, err := helmutils.ValuesFromSpec(mg)
	if err != nil {
		return fmt.Errorf("getting spec values: %w", err)
	}
	err = values.InjectGlobalValues(mg, h.pluralizer, krateoNamespace)
	if err != nil {
		return fmt.Errorf("injecting global values: %w", err)
	}
	postrenderLabels, err := utils.LabelPostRenderFromSpec(mg, h.pluralizer, krateoNamespace)
	if err != nil {
		return fmt.Errorf("creating label post renderer: %w", err)
	}
	// Cross-composition trace propagation (see Observe); excluded from the release digest.
	tpCarrier := map[string]string{}
	telemetry.InjectTraceparent(ctx, tpCarrier)
	postrenderLabels.WithTraceparent(tpCarrier[meta.AnnotationKeyTraceparent], tpCarrier[meta.AnnotationKeyTracestate])

	actionConfig := &helmconfig.ActionConfig{
		ChartVersion:          pkg.Version,
		ChartName:             pkg.Repo,
		Values:                values,
		Username:              pkg.Auth.Username,
		Password:              pkg.Auth.Password,
		InsecureSkipTLSverify: pkg.InsecureSkipTLSverify,
		PostRenderer:          postrenderLabels,
		// Adopt an existing child object rather than aborting the whole release when it carries
		// non-Helm ownership metadata (out-of-band-created/edited composition instance). Otherwise one
		// un-adoptable child 500s the entire reconcile and wedges the platform (D1); with it the
		// release takes ownership and self-heals the conflict.
		TakeOwnership: true,
	}

	// Check if the release already exists before attempting to install, this can happen if the create event is triggered after a failed install
	rel, err := hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return fmt.Errorf("finding helm release: %w", err)
	}
	helmMetrics = metrics.NewHelmMetrics(ctx)
	if rel != nil {
		log.Debug("Release already exists, upgrading instead of installing.")
		rel, err = helmMetrics.TimedUpgradeWithResult(func() (*helmconfig.Release, error) {
			return hc.Upgrade(ctx, releaseName, pkg.URL, &helmconfig.UpgradeConfig{
				ActionConfig: actionConfig,
				MaxHistory:   helmMaxHistory,
			})
		})
	} else {
		rel, err = helmMetrics.TimedInstallWithResult(func() (*helmconfig.Release, error) {
			return hc.Install(ctx, releaseName, pkg.URL, &helmconfig.InstallConfig{
				ActionConfig: actionConfig,
			})
		})
		if err != nil {
			return fmt.Errorf("installing helm chart: %w", err)
		}
	}

	log.Debug("Installing composition package", "package", pkg.URL)

	all, digest, err := processor.DecodeMinRelease(rel)
	if err != nil {
		return fmt.Errorf("decoding release: %w", err)
	}

	err = h.setStatus(ctx, mg, &statusManagerOpts{
		force:           true,
		resources:       all,
		previousDigest:  "",
		digest:          digest,
		message:         "Composition created",
		chartURL:        pkg.URL,
		chartVersion:    pkg.Version,
		releaseStatus:   string(rel.Status),
		releaseRevision: rel.Revision,
		releaseName:     rel.Name,
		// Creating, not Available: children cannot be healthy the instant Install returns; the first
		// post-install Observe runs the child-health rollup and promotes to Available (or Unavailable).
		conditionType: ConditionTypeCreating,
	})
	if err != nil {
		return fmt.Errorf("setting status: %w", err)
	}

	log.Debug("Composition created.", "package", pkg.URL)

	h.eventRecorder.Event(mg, event.Normal(reasonCreated, "Create", fmt.Sprintf("Composition created: %s", mg.GetName())))
	mg, err = updateStatusWithRetry(ctx, mg, updateOpts)
	if err != nil {
		return fmt.Errorf("updating cr with values: %w", err)
	}

	meta.RemoveAnnotations(mg, compositionMeta.AnnotationKeyReconciliationGracefullyPausedTime)
	_, err = tools.Update(ctx, mg, updateOpts)
	if err != nil {
		return fmt.Errorf("updating cr with values: %w", err)
	}

	return nil
}

func (h *handler) Update(ctx context.Context, mg *unstructured.Unstructured) error {
	mg = mg.DeepCopy()
	releaseName := compositionMeta.GetReleaseName(mg)

	log := xcontext.Logger(ctx)

	log = log.WithValues("op", "Update").
		WithValues("apiVersion", mg.GetAPIVersion()).
		WithValues("kind", mg.GetKind()).
		WithValues("name", mg.GetName()).
		WithValues("namespace", mg.GetNamespace())

	dyn, err := dynamic.NewForConfig(h.kubeconfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	updateOpts := tools.UpdateOptions{
		Pluralizer:    h.pluralizer,
		DynamicClient: dyn,
	}

	if _, p := compositionMeta.GetGracefullyPausedTime(mg); p && compositionMeta.IsGracefullyPaused(mg) {
		log.Debug("Composition is gracefully paused, skipping update.")
		h.eventRecorder.Event(mg, event.Normal(reasonReconciliationGracefullyPaused, "Update", "Reconciliation is paused via the gracefully paused annotation."))
		return nil
	}

	log.Debug("Handling composition update")

	if h.packageInfoGetter == nil {
		return fmt.Errorf("helm chart package info getter must be specified")
	}

	pkg, err := h.packageInfoGetter.WithLogger(log).Get(mg)
	if err != nil {
		return fmt.Errorf("getting package info: %w", err)
	}

	// Keep the CompositionDefinition def-ref labels current on the instance (mutating phase); Observe
	// no longer persists them (same rationale as Create).
	compositionMeta.SetCompositionDefinitionLabels(mg, compositionMeta.CompositionDefinitionInfo{
		Name:      pkg.CompositionDefinitionInfo.Name,
		Namespace: pkg.CompositionDefinitionInfo.Namespace,
		GVR:       pkg.CompositionDefinitionInfo.GVR,
	})
	if mg, err = tools.Update(ctx, mg, updateOpts); err != nil {
		return fmt.Errorf("stamping composition-definition labels: %w", err)
	}

	// Update the helm chart. Observe now renders as a dry-run (no revision written), so the live
	// mutation happens HERE — the runtime routes to Update only when Observe reported drift. Perform
	// the real Upgrade so the change is actually applied, mirroring the Observe render (same values,
	// global-value injection, post-render labels + traceparent, TakeOwnership).
	hc, err := helm.NewClient(h.kubeconfig,
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
	)
	if err != nil {
		return fmt.Errorf("creating helm client: %w", err)
	}

	values, err := helmutils.ValuesFromSpec(mg)
	if err != nil {
		return fmt.Errorf("getting spec values: %w", err)
	}
	err = values.InjectGlobalValues(mg, h.pluralizer, krateoNamespace)
	if err != nil {
		return fmt.Errorf("injecting global values: %w", err)
	}
	postrenderLabels, err := utils.LabelPostRenderFromSpec(mg, h.pluralizer, krateoNamespace)
	if err != nil {
		return fmt.Errorf("creating label post renderer: %w", err)
	}
	// Cross-composition trace propagation (see Observe); excluded from the release digest.
	tpCarrier := map[string]string{}
	telemetry.InjectTraceparent(ctx, tpCarrier)
	postrenderLabels.WithTraceparent(tpCarrier[meta.AnnotationKeyTraceparent], tpCarrier[meta.AnnotationKeyTracestate])

	helmMetrics := metrics.NewHelmMetrics(ctx)
	upgradedRel, err := helmMetrics.TimedUpgradeWithResult(func() (*helmconfig.Release, error) {
		return hc.Upgrade(ctx, releaseName, pkg.URL, &helmconfig.UpgradeConfig{
			ActionConfig: &helmconfig.ActionConfig{
				ChartVersion:          pkg.Version,
				ChartName:             pkg.Repo,
				Username:              pkg.Auth.Username,
				Password:              pkg.Auth.Password,
				InsecureSkipTLSverify: pkg.InsecureSkipTLSverify,
				Values:                values,
				PostRenderer:          postrenderLabels,
				TakeOwnership:         true,
			},
			MaxHistory: helmMaxHistory,
		})
	})
	if err != nil {
		return fmt.Errorf("upgrading helm chart: %w", err)
	}
	if upgradedRel == nil {
		log.Debug("Release not found after upgrade.")
		return fmt.Errorf("release not found after upgrade")
	}

	previousDigest, err := maps.NestedString(mg.Object, "status", "digest")
	if err != nil {
		return fmt.Errorf("getting previous digest from status: %w", err)
	}

	all, digest, err := processor.DecodeMinRelease(upgradedRel)
	if err != nil {
		return fmt.Errorf("decoding release: %w", err)
	}

	managed, err := h.populateManagedResources(all, mg.GetNamespace())
	if err != nil {
		return fmt.Errorf("populating managed resources: %w", err)
	}
	setManagedResources(mg, managed)

	log.Debug("Composition values updated.", "package", pkg.URL)

	h.eventRecorder.Event(mg, event.Normal(reasonUpdated, "Update", fmt.Sprintf("Updated composition: %s", mg.GetName())))

	statusOpts := &statusManagerOpts{
		force:           false,
		resources:       all,
		digest:          digest,
		previousDigest:  previousDigest,
		message:         "Composition values updated",
		chartURL:        pkg.URL,
		chartVersion:    pkg.Version,
		releaseStatus:   string(upgradedRel.Status),
		releaseRevision: upgradedRel.Revision,
		releaseName:     upgradedRel.Name,
		conditionType:   ConditionTypeAvailable,
	}
	err = h.setStatus(ctx, mg, statusOpts)
	if err != nil {
		return fmt.Errorf("setting status: %w", err)
	}

	mg, err = updateStatusWithRetry(ctx, mg, tools.UpdateOptions{
		Pluralizer:    h.pluralizer,
		DynamicClient: dyn,
	})
	if err != nil {
		return fmt.Errorf("updating cr status with values: %w", err)
	}

	if compositionMeta.IsGracefullyPaused(mg) {
		statusOpts.conditionType = ConditionTypeReconcileGracefullyPaused
		compositionMeta.SetGracefullyPausedTime(mg, time.Now())
		log.Debug("Composition gracefully paused.")
		h.eventRecorder.Event(mg, event.Normal(reasonReconciliationGracefullyPaused, "Update", "Reconciliation paused via the gracefully paused annotation."))

	} else {
		statusOpts.conditionType = ConditionTypeAvailable
		meta.RemoveAnnotations(mg, compositionMeta.AnnotationKeyReconciliationGracefullyPausedTime)
	}

	mg, err = tools.Update(ctx, mg, updateOpts)
	if err != nil {
		return fmt.Errorf("updating cr with values: %w", err)
	}
	return nil
}

func (h *handler) Delete(ctx context.Context, mg *unstructured.Unstructured) error {
	mg = mg.DeepCopy()

	releaseName := compositionMeta.GetReleaseName(mg)

	log := xcontext.Logger(ctx)

	log = log.WithValues("op", "Delete").
		WithValues("apiVersion", mg.GetAPIVersion()).
		WithValues("kind", mg.GetKind()).
		WithValues("name", mg.GetName()).
		WithValues("namespace", mg.GetNamespace())

	dyn, err := dynamic.NewForConfig(h.kubeconfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	updateOpts := tools.UpdateOptions{
		Pluralizer:    h.pluralizer,
		DynamicClient: dyn,
	}

	if _, p := compositionMeta.GetGracefullyPausedTime(mg); p && compositionMeta.IsGracefullyPaused(mg) {
		log.Debug("Composition is gracefully paused, skipping delete.")
		h.eventRecorder.Event(mg, event.Normal(reasonReconciliationGracefullyPaused, "Delete", "Reconciliation is paused via the gracefully paused annotation."))
		return nil
	}

	if h.packageInfoGetter == nil {
		return fmt.Errorf("helm chart package info getter must be specified")
	}

	hc, err := helm.NewClient(h.kubeconfig,
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
	)
	if err != nil {
		return fmt.Errorf("creating helm client: %w", err)
	}

	pkg, err := h.packageInfoGetter.WithLogger(log).Get(mg)
	if err != nil {
		return fmt.Errorf("getting package info: %w", err)
	}

	// Check if the release exists before uninstalling
	rel, err := hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return fmt.Errorf("finding helm release: %w", err)
	}
	if rel == nil {
		log.Debug("Release not found, nothing to uninstall.", "package", pkg.URL)
		h.eventRecorder.Event(mg, event.Normal(reasonDeleted, "Delete", fmt.Sprintf("Release not found, nothing to uninstall: %s", mg.GetName())))
		return nil
	}

	// GVK version-migration handover. When a composition's chart version is bumped, its CR's
	// apiVersion (composition.krateo.io/v<ver>) changes, so the umbrella prunes the old-GVK CR
	// and creates a new-GVK one — both bound (via the stable krateo.io/release-name label) to the
	// SAME helm release. This Delete fires for the pruned old-GVK CR. Uninstalling here would
	// destroy the release and its stateful children (e.g. ClickHouse/Keeper -> a fresh reinstall
	// that re-rolls them into ZK-auth staleness). Instead, detect the handover and skip the
	// uninstall: the new-version controller's Reconcile is install-or-upgrade, so it upgrades the
	// surviving release IN PLACE (helm upgrade, not uninstall+install). We detect it race-free via
	// the owning CompositionDefinition (resolved into pkg), whose spec.chart.version is bumped to the
	// new version BEFORE the old CR is pruned: CD.version != this CR's apiVersion version => migration.
	if isVersionMigrationHandover(mg, pkg) {
		log.Info("GVK version-migration handover detected; skipping uninstall so the new-version controller upgrades the release in place", "release", releaseName)
		h.eventRecorder.Event(mg, event.Normal(reasonDeleted, "Delete", fmt.Sprintf("GVK migration: release %s handed over to the new version for in-place helm upgrade; uninstall skipped", releaseName)))
		return nil
	}

	// A release stuck mid-uninstall (a prior Delete that died after helm began the uninstall) can never
	// be re-driven: helm refuses to operate on a release locked in `uninstalling`, so the Uninstall
	// below would fail every reconcile and the composition would wedge in Deleting forever. Mirror the
	// Observe recovery: within the grace period the uninstall may still be legitimately in flight (wait
	// and retry); past it, roll back to clear the lock, then re-drive the uninstall cleanly below.
	if rel.Status == helmconfig.StatusUninstalling {
		stuckFor := time.Since(rel.Updated)
		if stuckFor < pendingOperationGrace {
			log.Debug("Release uninstall in progress; waiting for it to settle before re-driving delete.",
				"stuckFor", stuckFor.String(), "grace", pendingOperationGrace.String())
			return fmt.Errorf("waiting for an in-flight uninstall of release %s to settle (stuck for %s)",
				releaseName, stuckFor.Truncate(time.Second))
		}
		log.Debug("Release stuck in uninstalling past the grace period; rolling back to clear the lock before re-driving the uninstall.",
			"stuckFor", stuckFor.String(), "grace", pendingOperationGrace.String())
		if _, rerr := hc.Rollback(ctx, releaseName, &helmconfig.RollbackConfig{
			MaxHistory:     helmMaxHistory,
			ReleaseVersion: rel.Revision,
		}); rerr != nil {
			return fmt.Errorf("clearing stuck uninstall of release %s before delete: %w", releaseName, rerr)
		}
	}

	helmMetrics := metrics.NewHelmMetrics(ctx)
	err = helmMetrics.TimedUninstall(func() error {
		return hc.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{
			IgnoreNotFound: true,
		})
	})
	if err != nil {
		return fmt.Errorf("uninstalling helm chart: %w", err)
	}

	rel, err = hc.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return fmt.Errorf("finding helm release: %w", err)
	}
	if rel != nil {
		return fmt.Errorf("composition not deleted, release %s still exists", releaseName)
	}

	log.Debug("Uninstalling RBAC", "package", pkg.URL)

	compositionGVR, err := h.pluralizer.GVKtoGVR(mg.GroupVersionKind())
	if err != nil {
		return fmt.Errorf("converting GVK to GVR: %w", err)
	}
	chartInspector := chartinspector.NewChartInspector(h.chartInspectorUrl)
	rbgen := metrics.WrapRBACGen(rbacgen.NewRBACGen(h.saName, h.saNamespace, &chartInspector))

	// Get Resources and generate RBAC
	generated, err := rbgen.
		WithBaseName(compositionMeta.GetReleaseName(mg)).
		Generate(ctx, rbacgen.Parameters{
			CompositionName:                mg.GetName(),
			CompositionNamespace:           mg.GetNamespace(),
			CompositionGVR:                 compositionGVR,
			CompositionDefinitionName:      pkg.CompositionDefinitionInfo.Name,
			CompositionDefinitionNamespace: pkg.CompositionDefinitionInfo.Namespace,
			CompositionDefintionGVR:        pkg.CompositionDefinitionInfo.GVR,
		})
	if err != nil {
		return fmt.Errorf("generating RBAC for composition %s/%s: %w",
			mg.GetNamespace(), mg.GetName(), err)
	}
	rbInstaller := rbac.NewRBACInstaller(dyn)
	err = helmMetrics.TimedRBAC(func() error {
		return rbInstaller.UninstallRBAC(generated)
	})
	if err != nil {
		return fmt.Errorf("uninstalling rbac: %w", err)
	}

	h.eventRecorder.Event(mg, event.Normal(reasonDeleted, "Delete", fmt.Sprintf("Deleted composition: %s", mg.GetName())))
	log.Debug("Composition package removed.", "package", pkg.URL)
	meta.RemoveAnnotations(mg, compositionMeta.AnnotationKeyReconciliationGracefullyPausedTime)

	_, err = tools.Update(ctx, mg, updateOpts)
	if err != nil {
		return fmt.Errorf("updating cr with values: %w", err)
	}

	return nil
}

// isVersionMigrationHandover reports whether this composition CR is being deleted as part of a
// chart-version bump (its GVK changing v<old> -> v<new>) rather than a genuine removal, in which
// case the helm release must survive for the new-version controller to upgrade in place.
//
// It compares this CR's own version — taken from its apiVersion (mg.GroupVersionKind().Version,
// e.g. "v0-1-7"), which is ALWAYS present and independent of any label — against the owning
// CompositionDefinition's current chart version. That version is pkg.Version: the getter resolves
// the owning CD (via the definition-ref labels when present, else a robust namespace search that
// tolerates label skew) and reads its spec.chart.version, so this check does NOT depend on the
// krateo.io/composition-definition-* / composition-version labels being present. Normalizing the
// CD's semver ("0.1.8" -> "v0-1-8") and comparing:
//   - CD version == this CR's version: no migration -> NOT a handover (uninstall).
//   - CD version != this CR's version: the definition already advanced to the new version while
//     this old-version CR is pruned -> handover (skip uninstall).
//
// This is race-free because the CompositionDefinition's version is advanced before the old CR is
// pruned. Crucially it is fail-safe: a failed install that never stamped the composition-definition-*
// labels no longer bails to "uninstall" — the owning CD is still resolved from the package info, so a
// version bump is detected and the destructive uninstall is skipped. When the owning CD genuinely
// cannot be resolved the package getter fails UPSTREAM (Delete returns an error and is retried)
// rather than reaching this check, so a version bump can never trigger a destructive uninstall.
func isVersionMigrationHandover(mg *unstructured.Unstructured, pkg *archive.Info) bool {
	if pkg == nil || pkg.Version == "" {
		return false
	}
	crVersion := mg.GroupVersionKind().Version
	if crVersion == "" {
		return false
	}
	// Normalize the owning CD's semver chart version ("0.1.8") to the CR's version form ("v0-1-8").
	cdVersion := "v" + strings.ReplaceAll(pkg.Version, ".", "-")
	return cdVersion != crVersion
}

// isIncompleteHelmOperation reports whether a release status indicates a helm operation that is
// either in flight or died mid-flight — helm labels both the same and never re-labels a crash. These
// are the statuses the stuck-operation recovery watches: a pending install/upgrade/rollback, or an
// uninstall that never completed (StatusUninstalling). Past the grace period the recovery rolls the
// release back to clear the stale lock, since helm refuses to operate on a release wedged in any of
// these states. StatusUninstalling is included so a Delete that died after helm began the uninstall
// (leaving the release locked in `uninstalling`) can self-recover instead of wedging forever.
func isIncompleteHelmOperation(status helmconfig.Status) bool {
	switch status {
	case helmconfig.StatusPendingInstall,
		helmconfig.StatusPendingUpgrade,
		helmconfig.StatusPendingRollback,
		helmconfig.StatusUninstalling:
		return true
	default:
		return false
	}
}

func (h *handler) getHelmLogger(verbose bool) func(format string, v ...interface{}) {
	if verbose {
		return func(format string, v ...interface{}) {
			slog.Debug(fmt.Sprintf(format, v...))
		}
	}
	return func(format string, v ...interface{}) {}
}
