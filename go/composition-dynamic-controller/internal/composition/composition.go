package composition

import (
	"context"
	"fmt"
	compositionCondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// childTeardownGrace bounds how long Delete waits for finalizer-bearing managed children to
	// finish deleting before it uninstalls the release anyway (krateo-core-provider#108). The wait
	// exists so a child can still reach its credentials while tearing down remote state; past the
	// grace we assume the child cannot finalize (remote already gone, credentials revoked, provider
	// down) and proceed, loudly, rather than holding the composition's own deletion open forever.
	childTeardownGrace = env.Duration(childTeardownGraceEnvvar, 5*time.Minute)
)

const (
	reasonReconciliationGracefullyPaused event.Reason = "ReconciliationGracefullyPaused"

	// Ready-condition reason used while a helm operation is still in flight and this reconcile is
	// deliberately standing down rather than starting a concurrent one.
	reasonPendingHelmOperation = "PendingHelmOperation"

	// Event reasons
	reasonCreated = "CompositionCreated"
	reasonDeleted = "CompositionDeleted"

	// Raised when finalizer-bearing children outlive the teardown grace: the composition proceeds to
	// uninstall, so their external resources may be left behind (#108). A Warning because abandoning
	// remote state is a real consequence, not routine.
	reasonChildTeardownAbandoned = "ChildTeardownAbandoned"

	// Raised when the pre-teardown RBAC refresh could not run (chart-inspector down, generation
	// failed). The teardown proceeds on the grants already in place, which may not be enough — a
	// Warning so a subsequent rollback/uninstall denial has a recorded cause rather than looking
	// like a fresh permissions mystery (#167).
	reasonRBACRefreshSkipped = "RBACRefreshSkipped"
	reasonUpdated            = "CompositionUpdated"

	// Environment variables
	helmMaxHistoryEnvvar     = "HELM_MAX_HISTORY"
	krateoNamespaceEnvVar    = "KRATEO_NAMESPACE"
	pendingGraceEnvvar       = "COMPOSITION_CONTROLLER_PENDING_GRACE"
	childTeardownGraceEnvvar = "COMPOSITION_CONTROLLER_CHILD_TEARDOWN_GRACE"

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
		cachedClients:      newHelmCachedClients(opts.Kubeconfig),
	}
}

// newHelmCachedClients builds the process-wide discovery client and RESTMapper for helm (#202).
// Degrade-only: a nil kubeconfig (tests) or a discovery failure returns nil, and every call site
// then behaves exactly as it did before this existed. Discovery is lazy inside
// DeferredDiscoveryRESTMapper, so this does not talk to the apiserver here.
func newHelmCachedClients(cfg *rest.Config) *helm.CachedClients {
	if cfg == nil {
		return nil
	}
	cc, err := helm.NewCachedClients(cfg)
	if err != nil {
		return nil
	}
	return &cc
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
	// cachedClients is ONE discovery client and RESTMapper for the whole process, handed to every
	// helm client this handler builds.
	//
	// Without it each helm.NewClient falls to plumbing's uncached RESTClientGetter, whose
	// ToRESTMapper performs a full API discovery -- every group, version and resource in the
	// cluster. Observe builds TWO clients and runs on every resync, so that was two full
	// discoveries per composition per tick. Measured on krateo-057 (#202): a controller managing
	// NOTHING spent ~38s of every 60s tick and allocated 5,190 GB over 16h, 2,466 GB of it (45.3%)
	// in NewDiscoveryRESTMapper.
	//
	// Built here rather than passed in, so no call site can forget it: the cost is invisible when
	// it is missing (a client that builds its own mapper behaves identically, only slower).
	//
	// Safe to share: DeferredDiscoveryRESTMapper guards its delegate with a mutex and the
	// memory-cached discovery client is synchronised, which matters because the fleet runs
	// workers=10. Staleness is handled by plumbing resetting the mapper on a REST-mapping miss --
	// when a lookup proves the cache stale, rather than on every CRD event in the cluster.
	//
	// nil when the kubeconfig is nil (tests) or discovery could not be built; every call site
	// passes it through helmCacheOpt, which omits the option in that case and so keeps the old
	// per-client behaviour rather than failing.
	cachedClients *helm.CachedClients
}

// helmCacheOpts is the shared-discovery option, or nothing when this handler has no cache.
//
// A SLICE rather than a single option because plumbing's ClientOption closes over an unexported
// type, so a caller outside that package cannot write a no-op one. Returning zero options is the
// honest degradation: plumbing refuses a nil *CachedClients by design (a stored nil would make a
// client look cached while every getter check fell through), so omitting the option entirely is
// how a caller says "I have no cache" and gets the previous per-client behaviour.
//
// Centralised so the five helm.NewClient sites cannot diverge.
func (h *handler) helmCacheOpts() []helm.ClientOption {
	if h.cachedClients == nil {
		return nil
	}
	return []helm.ClientOption{helm.WithCachedClients(h.cachedClients)}
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

		// #258. RETURNED, not written. Before this the pause was invisible on the object: an
		// already-paused composition kept whatever Ready it had, so one paused while Unavailable
		// went on reporting the FAILURE. Two sessions concluded the annotation was inert and halted
		// a live migration on exactly that, and a gate of "proceed when it reports paused" would
		// have waited forever on the one composition that had to be paused.
		//
		// Writing it here cannot work and must not be attempted: Observe deep-copies mg on entry
		// (see the first line of this function), so a condition set here is discarded — and the
		// runtime re-fetches the object right after Observe returns, so even mutating the shared
		// object would be thrown away. Returning it hands the condition to the runtime, which
		// applies it after that re-fetch and before its own status write (unstructured-runtime
		// v1.7.0). The handler never writes status, which is what makes the "Observe is a pure
		// read" rule above structural rather than a request.
		//
		// Status True: a pause is a deliberate operator action, not a failure, and keeping it True
		// means readiness-gating consumers are unaffected by someone pausing a composition.
		return controller.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: true,
			Conditions:       []metav1.Condition{compositionCondition.ReconcileGracefullyPaused()},
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

	hc, err := helm.NewClient(h.kubeconfig, append(h.helmCacheOpts(),
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
		helm.WithCache(),
	)...)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("creating helm client: %w", err)
	}
	// A helm client owns background resources and MUST be closed: WithCache() starts a DiskCache
	// cleanup goroutine (hourly ticker) whose only exit is Close(), and WithCRDInformer would add a
	// second. Observe builds TWO clients and runs on every resync, so never closing them leaked ~2
	// goroutines per reconcile per composition — permanently. That is the monotone, work-correlated
	// growth into the 256Mi limit in krateo-core-provider#99 (and why a controller with zero
	// compositions, which never reaches Observe, stayed flat).
	//
	// Value-captured on purpose: `defer hc.Close()` evaluates hc NOW, whereas a closure would
	// capture the variable and — since hc is reassigned below — close the second client twice.
	// DiskCache.Stop() closes an unguarded channel, so a double close panics.
	defer hc.Close()

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

	// RBAC FIRST — before any helm call, including the stuck-operation recovery below.
	//
	// Every helm call in this function runs under the composition's own ServiceAccount, so the
	// derived grants are a PRECONDITION of all of them. This block used to sit after the recovery,
	// which made a wedged release unrecoverable: the recovery's Rollback needs `get` on the chart's
	// resources, it fails without them, and the function returns before ever reaching the only call
	// that can repair the grants (core-provider#167). That is not hypothetical — a binding left
	// pointing at a deleted ServiceAccount after a version bump (core-provider#166) wedged the portal
	// composition for 22 hours, because the one block able to re-subject it was sequenced behind the
	// operation that needed it. ApplyRBAC can always fix this: the static baseline grants
	// rbac.authorization.k8s.io/{roles,rolebindings,clusterroles,clusterrolebindings}.
	if err := h.applyDerivedRBAC(ctx, mg, pkg, dyn, releaseName); err != nil {
		cond := condition.Unavailable()
		cond.Message = err.Error()
		unstructuredtools.SetConditions(mg, cond)
		if _, uerr := updateStatusWithRetry(ctx, mg, updateOpts); uerr != nil {
			return controller.ExternalObservation{}, fmt.Errorf("updating status after failure: %w", uerr)
		}
		return controller.ExternalObservation{}, err
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
		rel, err = helmPhase(log, "rollback", releaseName, func() (*helmconfig.Release, error) {
			return hc.Rollback(ctx, releaseName, &helmconfig.RollbackConfig{
				MaxHistory:     helmMaxHistory,
				ReleaseVersion: rel.Revision,
			})
		})
		if err != nil {
			return controller.ExternalObservation{}, fmt.Errorf("rolling back release: %w", err)
		}
	}

	tracer := tracer.NewTracer(ctx, meta.IsVerbose(mg))
	cfg := rest.CopyConfig(h.kubeconfig)
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return tracer.WithRoundTripper(rt)
	}
	// cfg is a COPY of h.kubeconfig with a traced transport, so it is the same cluster and the
	// shared discovery cache is correct here. One consequence worth knowing: that cache was built
	// from the untraced config, so discovery requests do not appear in the trace -- which keeps
	// the trace about the release rather than about API discovery.
	hc, err = helm.NewClient(cfg, append(h.helmCacheOpts(),
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithCache(),
	)...)
	if err != nil {
		return controller.ExternalObservation{}, fmt.Errorf("getting helm client: %w", err)
	}
	// Second client in this function (tracing transport) — its own DiskCache janitor, so its own
	// deferred close. See the note above on value capture: this closes THIS client, the earlier
	// defer closes the first one.
	defer hc.Close()

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
	// child composition's controller continues the same distributed trace. Excluded from the
	// release digest (processor.ComputeReleaseDigest) so it never churns.
	//
	// NOT a no-op when tracing is disabled, despite what you would expect — an earlier version of
	// this comment claimed it was, and a rollout was planned on that claim. telemetry.SetupTracing
	// installs the W3C propagator UNCONDITIONALLY, before its disabled return, so that an inbound
	// trace is honoured regardless of this process's export config; StartReconcileSpan extracts
	// krateo.io/traceparent from the object into ctx either way; and OTel's no-op tracer preserves
	// a valid REMOTE span context. So with tracing off:
	//
	//	object carries no traceparent  -> nothing injected, nothing stamped
	//	object carries a traceparent   -> it IS injected and children ARE stamped
	//
	// The second case propagates: children of a composition are themselves compositions, so one
	// traceparent entering anywhere walks down the whole tree through controllers that have
	// tracing disabled. Treat "no stamping" as a property of the fleet carrying no traceparent,
	// measurable at any time, and never as a consequence of the export config.
	tpCarrier := map[string]string{}
	telemetry.InjectTraceparent(ctx, tpCarrier)
	postrenderLabels.WithTraceparent(tpCarrier[meta.AnnotationKeyTraceparent], tpCarrier[meta.AnnotationKeyTracestate])
	// Self-healing "apply-if-changed" reconcile. Observe is no longer read-only nor an
	// unconditional live Upgrade (the old behaviour created an identical revision every ~60s cycle
	// even at steady state — infinite revision churn and needless API-server load, upstream
	// composition-dynamic-controller#184). Reconcile runs helm's 3-way merge
	// (KubeClient.UpdateThreeWayMerge) through the EXPORTED helm API every cycle: it recreates
	// children deleted out-of-band and patches drifted fields, converging the cluster — but only
	// writes a new helm revision + runs hooks when the live cluster was ACTUALLY mutated.
	//
	// "Actually mutated" is decided by comparing each child's content BEFORE and AFTER the merge
	// with write-volatile and server-owned fields stripped — explicitly NOT a resourceVersion
	// delta and NOT len(Result.Updated), both of which over-counted server-owned no-op re-applies
	// and churned a revision every cycle at steady state (plumbing helm/v3/reconcile.go steps 4-6).
	// An earlier version of this comment claimed the RV-delta WAS the gate; it was replaced
	// precisely because it over-counted, so do not build a prediction on it. At steady
	// state it is a no-op: no revision, no hooks. ResourceUpToDate reflects whether the cluster
	// already matched the desired state; the digest below is computed for STATUS REPORTING only,
	// not as an up-to-date gate.
	// #276 defect 1. Re-check for a deletion IMMEDIATELY before the apply. The guard at the top of
	// Observe is 252 lines upstream of this line, so a reconcile that passed it before a delete was
	// issued would otherwise self-heal — re-creating the children helm is concurrently uninstalling,
	// as orphans with no release annotation that nothing will ever remove.
	//
	// A FAILED RE-READ PROCEEDS, deliberately, and this is the opposite of the choice made in #270.
	// There, "could not confirm the pod is gone" had to refuse, because the destructive act was
	// revoking an identity. Here the destructive act would be HALTING every reconcile on a
	// transient read error, which is the wedge shape #270 was about. Proceeding degrades to exactly
	// the pre-#276 behaviour rather than to a new failure mode, and PR plumbing#60 makes any orphan
	// it produces non-fatal.
	if deleting, derr := isBeingDeleted(ctx, mg, updateOpts); derr != nil {
		log.Debug("could not re-check the composition for deletion before the self-heal; proceeding",
			"error", derr.Error())
	} else if deleting {
		log.Info("Composition was deleted while this reconcile was in flight; skipping the self-heal "+
			"so it does not re-create children the uninstall is removing", "package", pkg.URL)
		return controller.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: true,
		}, nil
	}

	reconcileRes, err := helmPhase(log, "reconcile", releaseName, func() (*helmconfig.ReconcileResult, error) {
		return hc.Reconcile(ctx, releaseName, pkg.URL, &helmconfig.UpgradeConfig{
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
		// Info, not Debug (#175). This is the ONLY line that says a composition drifted and what
		// was done about it, and at Debug it was invisible on every cluster that runs at Info —
		// which is why 62,783 revisions on git-provider went unnoticed for 28 days. A self-heal is
		// a real event about the cluster, not controller noise, and it is bounded by how often
		// children actually drift rather than by the reconcile cadence.
		log.Info("Composition drift detected and self-healed", "package", pkg.URL,
			"created", reconcileRes.Created, "deleted", reconcileRes.Deleted, "patched", reconcileRes.PatchedUpdated)
		return controller.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	// #261: a child recorded in status.managed but ABSENT from the cluster is drift by definition —
	// either the record is stale or the child needs recreating, and both are Update's job. Observe
	// cannot repair it: only Create/Update write status.managed, and getting the current render here
	// would cost a full manifest decode on every reconcile (measured: 15-27x the digest-only path
	// and 230KB/11.5MB/125MB of allocation for a 10KB/500KB/5MB manifest, against 32-40 bytes).
	//
	// So report drift and let Update — which already decodes the upgraded release and rewrites the
	// recorded set from it — do the repair. Costs nothing per reconcile: the rollup already performs
	// these GETs to compute Ready.
	var absentChildren []string

	err = h.setStatus(ctx, mg, &statusManagerOpts{
		force:           false,
		resources:       nil, // we don't need to set resources here as they are already set when a resource is created/updated
		absentChildren:  &absentChildren,
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

	if len(absentChildren) > 0 {
		// Not an error and not logged as one: the common cause is a child applied moments ago that
		// has not appeared yet, where the Update this triggers is a cheap no-op and records the
		// same set again. The expensive case is the one worth paying for — a chart that RENAMED a
		// child leaves the old name recorded forever, because the digest is unchanged so nothing
		// would otherwise dispatch an Update (#261).
		log.Info("Composition reports managed children that do not exist; reporting drift so the "+
			"recorded set is rewritten", "absent", absentChildren)
		return controller.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
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
	// Freeze this CR's origin version (#168). At FIRST reconcile the delivered apiVersion is the
	// version the object was actually written at, so this is the one moment it can be captured
	// truthfully; after a chart-version bump re-points the CRD the apiVersion no longer says
	// anything about where the object came from. Write-once, so a later reconcile through a
	// re-pointed endpoint cannot overwrite it.
	compositionMeta.SetOriginVersion(mg, mg.GroupVersionKind().Version)
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
		// #265. Name whose fault this is. A blueprint referencing a kind the cluster does not serve
		// is a 422 and resolves itself once the CRD exists; chart-inspector failing is a 5xx and is
		// not the blueprint's problem. Both used to arrive folded into Synced's message as an
		// indistinguishable 500. Best-effort: the condition is a report, so failing to write it
		// must not replace the real error with a reporting error.
		reason, message := chartRenderableFailure(err)
		if cerr := setChartRenderableCondition(mg, metav1.ConditionFalse, reason, message); cerr != nil {
			xcontext.Logger(ctx).Info("setting ChartRenderable condition", "error", cerr.Error())
		}
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

	hc, err := helm.NewClient(h.kubeconfig, append(h.helmCacheOpts(),
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
	)...)
	if err != nil {
		return fmt.Errorf("creating helm client: %w", err)
	}
	// Close the client's background resources — see the note in Observe (#99). This client is
	// built without WithCache(), so there is no janitor to stop today; closing is still correct
	// and keeps the leak from reappearing if caching is ever enabled on this path.
	defer hc.Close()

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
		rel, err = helmPhase(log, "upgrade", releaseName, func() (*helmconfig.Release, error) {
			return helmMetrics.TimedUpgradeWithResult(func() (*helmconfig.Release, error) {
				return hc.Upgrade(ctx, releaseName, pkg.URL, &helmconfig.UpgradeConfig{
					ActionConfig: actionConfig,
					MaxHistory:   helmMaxHistory,
				})
			})
		})
		// Unchecked, a failed upgrade left rel nil and the status write below dereferenced it: the
		// controller panicked mid-reconcile. Reproduced on a fresh vcluster child, where the portal's
		// first install outlived pendingOperationGrace, was rolled back, and this upgrade then ran
		// against a release still locked pending-rollback. Returning the error lets the next
		// reconcile's Observe clear the lock instead.
		if err != nil {
			return fmt.Errorf("upgrading existing helm release: %w", err)
		}
	} else {
		rel, err = helmPhase(log, "install", releaseName, func() (*helmconfig.Release, error) {
			return helmMetrics.TimedInstallWithResult(func() (*helmconfig.Release, error) {
				return hc.Install(ctx, releaseName, pkg.URL, &helmconfig.InstallConfig{
					ActionConfig: actionConfig,
				})
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
		recordManaged:   true,
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
	// Backfill the origin stamp for compositions that predate it (#168), so the fleet stops
	// depending on the old race within one reconcile rather than only at its next bump.
	//
	// The value written here is the delivered apiVersion, which for an object mid-bump may be the
	// NEW version rather than its true origin. That is not a regression: an un-stamped object
	// already falls back to exactly this value, so the worst case reproduces today's behaviour
	// for the bump in flight and is correct for every bump after it. Write-once means a correct
	// stamp can never be spoiled by a later one.
	compositionMeta.SetOriginVersion(mg, mg.GroupVersionKind().Version)
	if mg, err = tools.Update(ctx, mg, updateOpts); err != nil {
		return fmt.Errorf("stamping composition-definition labels: %w", err)
	}

	// Converge the helm release. Reconcile, NOT Upgrade (#175).
	//
	// The comment that stood here said Observe renders as a dry-run so the live mutation happens
	// here. That stopped being true: Observe calls hc.Reconcile, which converges the cluster and
	// writes a revision when it actually changed something. By the time the runtime routes here —
	// which it does precisely BECAUSE Observe reported drift — the drift is already healed, and an
	// unconditional Upgrade wrote a second revision for it.
	//
	// Measured on krateo-057: strictly bimodal pairs, ~3s apart within a pair and ~58s between
	// pairs, against a 60s reconcile cadence. git-provider reached 62,783 revisions over 28 days
	// and cloud-meetup-turin sat at 99.8% of the theoretical 2.00/min ceiling. One drifting child,
	// two revisions and two helm operations per cycle, indefinitely.
	//
	// Reconcile removes the doubling STRUCTURALLY rather than by suppressing this call: it is
	// apply-if-changed, so when Observe has already converged the cluster it writes no revision and
	// runs no hooks. A genuine change still converges — once. No signal has to pass from Observe to
	// Update, which is what makes this robust to either phase changing later.
	//
	// Suppressing the dispatch instead (returning ResourceUpToDate on self-heal) was the obvious
	// alternative and is wrong: Update is the ONLY place that keeps the def-ref labels current,
	// backfills the #168 origin stamp, and refreshes status.managed. A composition that only ever
	// self-heals — alert-provider, whose Job is deleted by ttlSecondsAfterFinished every 10
	// minutes — would silently stop getting all three.
	//
	// Same values, global-value injection, post-render labels + traceparent and TakeOwnership as
	// the Observe reconcile, so the two agree on the desired state and the second genuinely is a
	// no-op.
	hc, err := helm.NewClient(h.kubeconfig, append(h.helmCacheOpts(),
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
	)...)
	if err != nil {
		return fmt.Errorf("creating helm client: %w", err)
	}
	// Close the client's background resources — see the note in Observe (#99). This client is
	// built without WithCache(), so there is no janitor to stop today; closing is still correct
	// and keeps the leak from reappearing if caching is ever enabled on this path.
	defer hc.Close()

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
	var reconcileRes *helmconfig.ReconcileResult
	upgradedRel, err := helmPhase(log, "upgrade", releaseName, func() (*helmconfig.Release, error) {
		return helmMetrics.TimedUpgradeWithResult(func() (*helmconfig.Release, error) {
			res, rerr := hc.Reconcile(ctx, releaseName, pkg.URL, &helmconfig.UpgradeConfig{
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
			if rerr != nil {
				return nil, rerr
			}
			reconcileRes = res
			return res.Release, nil
		})
	})
	if err != nil {
		return fmt.Errorf("upgrading helm chart: %w", err)
	}
	if upgradedRel == nil {
		log.Debug("Release not found after upgrade.")
		return fmt.Errorf("release not found after upgrade")
	}
	// Changed==false is the normal, desired outcome: Observe's reconcile already converged the
	// cluster, so this one had nothing to do and wrote no revision. Logged at Info when it DID
	// write, because that is the case worth seeing — it means Observe's convergence did not hold,
	// or a change arrived between the two phases.
	if reconcileRes != nil && reconcileRes.Changed {
		log.Info("Update wrote a helm revision; Observe's reconcile had not fully converged",
			"package", pkg.URL, "revision", upgradedRel.Revision,
			"created", reconcileRes.Created, "deleted", reconcileRes.Deleted,
			"patched", reconcileRes.PatchedUpdated)
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

	// Decided BEFORE setStatus, and that ordering is the whole of #232. Pausedness selects the
	// conditionType, and setStatus ALSO uses the conditionType to decide whether to evaluate the
	// statusDataTemplate projection — so a decision taken after setStatus can reach neither. It used
	// to be taken after, by assigning statusOpts.conditionType at a point where statusOpts is never
	// read again, which made both effects unreachable: ConditionTypeReconcileGracefullyPaused was
	// never passed to setStatus from anywhere, so setGracefullyPausedCondition could not be called
	// and the projection gate could not fire. A paused composition reported Ready=True/Available and
	// had its status projected as if it were reconciling.
	//
	// Checked here rather than at the top of Update because Observe already returns early when
	// paused: reaching this point at all means the annotation arrived DURING this reconcile, and the
	// latest read of it is the useful one.
	paused := compositionMeta.IsGracefullyPaused(mg)

	statusOpts := &statusManagerOpts{
		force:           false,
		resources:       all,
		recordManaged:   true,
		digest:          digest,
		previousDigest:  previousDigest,
		message:         "Composition values updated",
		chartURL:        pkg.URL,
		chartVersion:    pkg.Version,
		releaseStatus:   string(upgradedRel.Status),
		releaseRevision: upgradedRel.Revision,
		releaseName:     upgradedRel.Name,
		conditionType:   convergedConditionType(paused),
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

	// The ANNOTATION work has to stay here, after the status update, even though the decision above
	// moved earlier. updateStatusWithRetry returns the server's response to a status-subresource
	// write, which does not carry local metadata edits — so annotations set on mg before it are
	// silently dropped, and only edits applied to the returned object survive tools.Update below.
	if paused {
		compositionMeta.SetGracefullyPausedTime(mg, time.Now())
		log.Debug("Composition gracefully paused.")
		h.eventRecorder.Event(mg, event.Normal(reasonReconciliationGracefullyPaused, "Update", "Reconciliation paused via the gracefully paused annotation."))
	} else {
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

	hc, err := helm.NewClient(h.kubeconfig, append(h.helmCacheOpts(),
		helm.WithNamespace(mg.GetNamespace()),
		helm.WithLogger(h.getHelmLogger(meta.IsVerbose(mg))),
	)...)
	if err != nil {
		return fmt.Errorf("creating helm client: %w", err)
	}
	// Close the client's background resources — see the note in Observe (#99). This client is
	// built without WithCache(), so there is no janitor to stop today; closing is still correct
	// and keeps the leak from reappearing if caching is ever enabled on this path.
	defer hc.Close()

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
	// surviving release IN PLACE (helm upgrade, not uninstall+install).
	//
	// Note "the umbrella prunes": this whole scenario requires a PARENT that re-renders. A
	// standalone composition is never deleted by a bump, so it cannot reach the handover branch —
	// see isVersionMigrationHandover. An earlier version of this comment called the detection
	// "race-free" via the CD's chart version; it was not, it was race-DEPENDENT on whether the CRD
	// re-point beat the Delete, and silent when it lost (portal 1.8.70 -> 1.8.71 lost it and the
	// release was uninstalled). The origin stamp is what makes the outcome the same either way.
	if isVersionMigrationHandover(mg, pkg) {
		log.Info("GVK version-migration handover detected; skipping uninstall so the new-version controller upgrades the release in place", "release", releaseName)
		h.eventRecorder.Event(mg, event.Normal(reasonDeleted, "Delete", fmt.Sprintf("GVK migration: release %s handed over to the new version for in-place helm upgrade; uninstall skipped", releaseName)))
		return nil
	}

	// RBAC FIRST — before the recovery below and the uninstall that follows it. Both run under this
	// composition's ServiceAccount and both need the chart's OWN kinds: the rollback `get`s every
	// resource in the manifest, the uninstall deletes them. If the binding has drifted off a replaced
	// ServiceAccount (core-provider#166), every route out of here is denied and the composition loops
	// on rollback->uninstall forever. Observe cannot help: it returns at the deletionTimestamp check,
	// so this handler is the ONLY one that can repair the grants of a composition being deleted
	// (core-provider#167).
	//
	// Deliberately NOT fatal, unlike the Observe path. A teardown must not become impossible because
	// chart-inspector is unavailable — that trades one wedge for another. Say so and carry on with
	// whatever grants are already in place; the uninstall may well succeed on them.
	if err := h.applyDerivedRBAC(ctx, mg, pkg, dyn, releaseName); err != nil {
		log.Info("Could not refresh derived RBAC before teardown; continuing with the grants already in place",
			"release", releaseName, "error", err.Error())
		h.eventRecorder.Event(mg, event.Warning(reasonRBACRefreshSkipped, "Delete", err))
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
		if _, rerr := helmPhase(log, "rollback", releaseName, func() (*helmconfig.Release, error) {
			return hc.Rollback(ctx, releaseName, &helmconfig.RollbackConfig{
				MaxHistory:     helmMaxHistory,
				ReleaseVersion: rel.Revision,
			})
		}); rerr != nil {
			return fmt.Errorf("clearing stuck uninstall of release %s before delete: %w", releaseName, rerr)
		}
	}

	// Ordered teardown (#108). helm deletes unknown kinds LAST ("unknown kind is last", helm's
	// kind_sorter.go), so a plain uninstall removes the credential Secret at position 32 of
	// UninstallOrder BEFORE the custom resources that need those credentials to delete their remote
	// state — stranding them with held finalizers and no parent left to repair them. Drain the
	// finalizer-bearing children first, while their dependencies still exist.
	if pending := h.drainFinalizerBoundChildren(ctx, dyn, mg); len(pending) > 0 {
		waited := drainedFor(pending, time.Now())
		if waited < childTeardownGrace {
			// Requeue: the composition keeps its finalizer and Delete is re-invoked. This is a
			// bounded version of the hot loop these children are already stuck in today.
			log.Debug("Waiting for managed children to finalize before uninstalling.",
				"pending", len(pending), "children", pendingIDs(pending),
				"waited", waited.String(), "grace", childTeardownGrace.String())
			return waitingForChildrenErr(pending, waited)
		}
		// Past the grace the child cannot finalize on its own (remote already gone, credentials
		// revoked, provider down). Proceed rather than hold the composition's deletion open forever —
		// but say so, because the remote state is now being abandoned rather than cleaned up.
		log.Info("Managed children did not finalize within the grace period; uninstalling anyway — their remote state may be left behind",
			"pending", len(pending), "children", pendingIDs(pending), "grace", childTeardownGrace.String())
		h.eventRecorder.Event(mg, event.Warning(reasonChildTeardownAbandoned, "Delete", fmt.Errorf(
			"%d managed child(ren) did not finalize within %s (%s); uninstalling anyway, their external resources may be orphaned",
			len(pending), childTeardownGrace, pendingIDs(pending))))
	}

	helmMetrics := metrics.NewHelmMetrics(ctx)
	err = helmPhaseErr(log, "uninstall", releaseName, func() error {
		return helmMetrics.TimedUninstall(func() error {
			return hc.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{
				IgnoreNotFound: true,
			})
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

// applyDerivedRBAC derives this composition's RBAC from its chart (via chart-inspector) and applies
// it. Applying also RE-SUBJECTS the generated bindings to the controller's CURRENT ServiceAccount,
// which is the part that is easy to miss and the reason this is called before any helm operation.
//
// The generated objects are RELEASE-scoped (`<release>-krateo-rbac`, stable across versions), while
// the ServiceAccount is {resource}-{apiVersion}-scoped and is REPLACED on every chart bump. Nothing
// else repoints the binding, so between a bump and the next successful apply the composition holds a
// binding naming a ServiceAccount that no longer exists — RBAC is name-based, so it resolves against
// nothing and the controller silently runs on the static baseline alone (core-provider#166).
func (h *handler) applyDerivedRBAC(ctx context.Context, mg *unstructured.Unstructured, pkg *archive.Info, dyn dynamic.Interface, releaseName string) error {
	compositionGVR, err := h.pluralizer.GVKtoGVR(mg.GroupVersionKind())
	if err != nil {
		return fmt.Errorf("converting GVK to GVR: %w", err)
	}

	chartInspector := chartinspector.NewChartInspector(h.chartInspectorUrl)
	rbgen := metrics.WrapRBACGen(rbacgen.NewRBACGen(h.saName, h.saNamespace, &chartInspector))
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
		// #265. Name whose fault this is. A blueprint referencing a kind the cluster does not serve
		// is a 422 and resolves itself once the CRD exists; chart-inspector failing is a 5xx and is
		// not the blueprint's problem. Both used to arrive folded into Synced's message as an
		// indistinguishable 500. Best-effort: the condition is a report, so failing to write it
		// must not replace the real error with a reporting error.
		reason, message := chartRenderableFailure(err)
		if cerr := setChartRenderableCondition(mg, metav1.ConditionFalse, reason, message); cerr != nil {
			xcontext.Logger(ctx).Info("setting ChartRenderable condition", "error", cerr.Error())
		}
		return fmt.Errorf("generating RBAC using chart-inspector: %w", err)
	}

	rbInstaller := rbac.NewRBACInstaller(dyn)
	if err := metrics.NewHelmMetrics(ctx).TimedRBAC(func() error {
		return rbInstaller.ApplyRBAC(generated)
	}); err != nil {
		return fmt.Errorf("applying rbac: %w", err)
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
// # Where this CR's "own version" comes from, and why not the apiVersion (#168)
//
// The delivered apiVersion is NOT this CR's origin. core-provider re-points the composition CRD to
// serve a single version, so after a bump an object written at v1-8-70 reads back as v1-8-71 —
// and when that re-point lands before the old-version controller has processed the Delete, the
// comparison below degenerates to cdVersion == crVersion and the DESTRUCTIVE branch runs on what
// was only a version bump. Both outcomes were observed on krateo-057: `frontend` 1.8.1 -> 1.8.2
// won the race and was handed over correctly; `portal` 1.8.70 -> 1.8.71 lost it and the release
// was uninstalled. An earlier revision of this comment claimed the detection was "race-free". It
// was not: it was race-DEPENDENT, and silent when it lost, which is why it looked correct for so
// long.
//
// So the origin is read from the krateo.io/composition-origin-version annotation, stamped once at
// first reconcile and never rewritten. krateo.io/composition-version cannot serve this purpose —
// the version MutatingAdmissionPolicy re-stamps it on every write, by design.
//
// Objects that predate the stamp fall back to the delivered apiVersion, i.e. to the old
// race-dependent behaviour, because the alternative — treating "no annotation" as a handover —
// would leak a release on every genuine deletion of an un-stamped composition. The fallback is
// narrow: Create and Update stamp the annotation, so a parent-rendered object keeps the old
// behaviour only until its next reconcile. (It is NOT self-closing in general, as an earlier
// version of this comment claimed: the runtime routes to Update only when Observe reports drift,
// so a composition sitting at steady state is never stamped at all.)
//
// # Why a standalone composition returns early (#199)
//
// The stamp is write-once while the CD's chart version keeps moving, so on a long-lived object the
// comparison below eventually reads "different" permanently and skips EVERY genuine uninstall,
// orphaning the release and all its children. That staleness can only build up where nothing
// recreates the object, which is exactly where a handover is impossible — so the fix is not a
// cleverer version comparison but declining to make one. See compositionMeta.IsParentRendered.
//
// Three shapes were tried and rejected before this one, each fixing one case by breaking the
// other: cd != origin leaks on a long-lived standalone object; cd != delivered restores the
// destructive uninstall; and making the stamp track the last reconcile instead of the first
// reintroduces the race, because the new-version controller can advance it before the old
// controller processes the Delete. No arrangement of origin/delivered/cd satisfies both
// requirements at once — a frozen value cannot pass a steady-state equality test, and a moving one
// cannot be trusted during the window where it moves.
//
// Fail-safe in the other direction too: a failed install that never stamped the
// composition-definition-* labels does not bail to "uninstall" — the owning CD is still resolved
// from the package info, so a version bump is still detected. When the owning CD genuinely cannot
// be resolved the package getter fails UPSTREAM (Delete returns an error and is retried) rather
// than reaching this check.
func isVersionMigrationHandover(mg *unstructured.Unstructured, pkg *archive.Info) bool {
	if pkg == nil || pkg.Version == "" {
		return false
	}
	// A handover requires something to hand over TO, and only a parent-rendered composition has
	// one: its parent re-renders at the new GVK, so helm deletes this object and creates the
	// replacement. A standalone composition is never deleted by a bump — the CRD is re-pointed
	// underneath the object it already has — so every delete it receives is a real teardown and
	// must uninstall (#199). See compositionMeta.IsParentRendered for the evidence.
	//
	// This is also what keeps the origin stamp honest. The stamp is written once and never moves,
	// while the CD's chart version keeps advancing, so on a long-lived object the comparison below
	// eventually reads "different" forever and would skip every genuine uninstall. That staleness
	// can only accumulate on a standalone object, because a parent-rendered one is recreated at
	// each bump and re-stamped at Create. Returning early here means the stale value is never the
	// value consulted.
	if !compositionMeta.IsParentRendered(mg) {
		return false
	}
	// The origin stamp when present; the delivered apiVersion only as a fallback for objects
	// written before the stamp existed.
	crVersion := compositionMeta.GetOriginVersion(mg)
	if crVersion == "" {
		crVersion = mg.GroupVersionKind().Version
	}
	if crVersion == "" {
		return false
	}

	// Is the stamp still TRUE of this object? (#199 residual.)
	//
	// A second question, not a second opinion on the first. IsParentRendered above asks whether
	// there is anything to hand over TO — a structural fact about who renders this object. This
	// asks whether the stamp we are about to compare still describes it.
	//
	// The claim the early return rests on is that a parent-rendered composition is RECREATED at
	// each bump and re-stamped at Create, so its stamp cannot go stale. That holds for a bump
	// delivered by its parent. It does not hold for one delivered OUT OF BAND, past the umbrella's
	// pin: then core-provider re-points the CRD under the object it already has, exactly as it
	// does for a standalone composition, and the write-once stamp freezes while the served version
	// walks away from it. IsParentRendered still says true, the stale stamp is consulted after
	// all, and a genuine delete reads as a migration and skips the uninstall — the #199 defect in
	// the one population #199 exempted.
	//
	// krateo.io/composition-version answers it directly and is the only field that can: the
	// version MutatingAdmissionPolicy re-stamps it on EVERY write, which is what makes it useless
	// as an origin (see AnnotationKeyOriginVersion) and exactly right as a freshness probe. Equal
	// to the stamp means the object has not been re-pointed since it was stamped. Different means
	// it has, and the stamp no longer describes it.
	//
	// Absent means the policy did not run — an old object, or a cluster without it. Then there is
	// no freshness evidence either way, and the guard yields rather than inventing a verdict: the
	// behaviour is exactly what it was before this check existed.
	if served := mg.GetLabels()[compositionMeta.CompositionVersionLabel]; served != "" && served != crVersion {
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
