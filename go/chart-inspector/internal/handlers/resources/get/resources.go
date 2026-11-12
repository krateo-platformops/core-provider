package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	coreprovv1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/unstructured-runtime/pkg/meta"

	"github.com/krateo-platformops/chart-inspector/internal/getter"
	"github.com/krateo-platformops/chart-inspector/internal/handlers"
	"github.com/krateo-platformops/chart-inspector/internal/handlers/resources"
	"github.com/krateo-platformops/chart-inspector/internal/helper"
	"github.com/krateo-platformops/chart-inspector/internal/tracer"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmutils "github.com/krateo-platformops/plumbing/helm/utils"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"github.com/krateo-platformops/plumbing/http/response"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	AnnotationKeyReconciliationGracefullyPaused = "krateo.io/gracefully-paused"
)

type handler struct {
	handlers.HandlerOptions
}

func GetResources(opts handlers.HandlerOptions) http.Handler {
	return &handler{
		HandlerOptions: opts,
	}
}

var _ http.Handler = (*handler)(nil)

// @Summary Get Helm chart resources
// @Description Get Helm chart resources
// @ID get-chart-resources
// @Param compositionName query string true "Composition name"
// @Param compositionNamespace query string true "Composition namespace"
// @Param compositionDefinitionName query string true "Composition definition name"
// @Param compositionDefinitionNamespace query string true "Composition definition namespace"
// @Param compositionDefinitionGroup query string false "Composition definition group" default(core.krateo.io)
// @Param compositionDefinitionVersion query string false "Composition definition version" default(v1alpha1)
// @Param compositionDefinitionResource query string false "Composition definition resource name" default(compositiondefinitions)
// @Param compositionGroup query string false "Composition group" default(composition.krateo.io)
// @Param compositionVersion query string true "Composition version"
// @Param compositionResource query string true "Composition resource name"
// @Produce json
// @Success 200 {object} []Resource
// @Router /resources [get]
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	compositionName := r.URL.Query().Get("compositionName")
	compositionNamespace := r.URL.Query().Get("compositionNamespace")
	compositionDefinitionName := r.URL.Query().Get("compositionDefinitionName")
	compositionDefinitionNamespace := r.URL.Query().Get("compositionDefinitionNamespace")
	compositionVersion := r.URL.Query().Get("compositionVersion")
	compositionResource := r.URL.Query().Get("compositionResource")
	releaseName := r.URL.Query().Get("releaseName")

	compositionGroup := helper.GetQueryParamWithDefault(r, "compositionGroup", "composition.krateo.io")
	compositionDefinitionGroup := helper.GetQueryParamWithDefault(r, "compositionDefinitionGroup", "core.krateo.io")
	compositionDefinitionVersion := helper.GetQueryParamWithDefault(r, "compositionDefinitionVersion", "v1alpha1")
	compositionDefinitionResource := helper.GetQueryParamWithDefault(r, "compositionDefinitionResource", "compositiondefinitions")

	log := h.Log.With(slog.String(
		"compositionName", compositionName),
		slog.String("compositionNamespace", compositionNamespace),
		slog.String("compositionDefinitionName", compositionDefinitionName),
		slog.String("compositionDefinitionNamespace", compositionDefinitionNamespace))

	if compositionName == "" || compositionNamespace == "" || compositionDefinitionName == "" || compositionDefinitionNamespace == "" || compositionVersion == "" || compositionResource == "" {
		log.ErrorContext(ctx, "missing required query parameters")
		response.BadRequest(w, fmt.Errorf("missing required query parameters"))
		return
	}

	// releaseName is REQUIRED and is checked SEPARATELY so the error names it (#329).
	//
	// This used to be read off the composition's krateo.io/release-name LABEL, from the live object
	// fetched below. That couples this service to the CALLER'S PERSISTENCE SCHEDULE, which is not
	// something a callee can see — and on core-provider's #303 adoption path the label does not
	// exist yet: a recreated CR has none (its absence is what triggers adoption), the cdc resolves
	// the adopted name in memory, and its Observe is a pure read that must not persist. So the
	// Install below ran with release name "" and helm answered `no name provided`, as a 500.
	//
	// A 500 is a claim about THIS SERVER. The caller then read "not my fault, retry later" and
	// retried at 1s, 2s, 4s, 8s, 16s forever, while the one sentence that resolved it sat in the
	// response body — and because the failure was in Observe, the caller never reached the phase
	// that would have written the label. The retry could not have worked.
	//
	// 400 WITH THE PARAMETER NAMED, not a silent fall back to the label. Falling back is what hid
	// this: an empty release name is indistinguishable from a caller that did not send one, and
	// guessing turns a caller's bug into this service's 500. See #325 for the same shape with a
	// different cause.
	if releaseName == "" {
		log.ErrorContext(ctx, "missing required query parameter releaseName")
		response.BadRequest(w, fmt.Errorf("missing required query parameter \"releaseName\": the "+
			"caller must send the helm release name it resolved for this composition. It is NOT "+
			"derived from the composition's krateo.io/release-name label here, because that label "+
			"may not be persisted yet on an adoption path (core-provider#329)"))
		return
	}

	k8scli := getter.NewClient(
		h.DynamicClient,
	)

	compositionGVR := schema.GroupVersionResource{
		Group:    compositionGroup,
		Version:  compositionVersion,
		Resource: compositionResource,
	}
	compositionDefinitionGVR := schema.GroupVersionResource{
		Group:    compositionDefinitionGroup,
		Version:  compositionDefinitionVersion,
		Resource: compositionDefinitionResource,
	}

	log.InfoContext(ctx, "Handling request to get resources")

	composition, err := h.DynamicClient.
		Resource(compositionGVR).
		Namespace(compositionNamespace).
		Get(ctx, compositionName, v1.GetOptions{})
	if err != nil {
		log.ErrorContext(ctx, "unable to get composition",
			slog.String("compositionName", compositionName),
			slog.String("compositionNamespace", compositionNamespace),
			slog.String("compositionVersion", compositionVersion),
			slog.String("compositionResource", compositionResource),
			slog.String("compositionGroup", compositionGroup),
			slog.Any("err", err),
		)
		response.InternalError(w, err)
		return
	}

	// NOTE: bValues are extracted and injected with composition context
	bValuesMap, err := helmutils.ValuesFromSpec(composition)
	if err != nil {
		log.ErrorContext(ctx, "unable to extract values from composition",
			slog.Any("err", err),
		)
		response.InternalError(w, err)
		return
	}
	err = bValuesMap.InjectGlobalValues(composition, h.Plurarizer, h.KrateoNamespace)
	if err != nil {
		log.ErrorContext(ctx, "unable to inject global values",
			slog.Any("err", err),
		)
		response.InternalError(w, err)
		return
	}

	tracer := &tracer.Tracer{}
	// Create a wrapped REST config with the tracer RoundTripper for this request
	wrappedCfg := rest.CopyConfig(h.RestConfig)
	wrappedCfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return tracer.WithRoundTripper(rt)
	}

	compositionDefinitionU, err := h.DynamicClient.
		Resource(compositionDefinitionGVR).
		Namespace(compositionDefinitionNamespace).
		Get(ctx, compositionDefinitionName, v1.GetOptions{})
	if err != nil {
		log.ErrorContext(ctx, "unable to get composition definition",
			slog.Any("err", err),
		)
		response.InternalError(w, err)
		return
	}
	var compositionDefinition coreprovv1.CompositionDefinition
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(compositionDefinitionU.Object, &compositionDefinition)
	if err != nil {
		log.ErrorContext(ctx, "unable to convert composition definition",
			slog.Any("err", err),
		)
		response.InternalError(w, err)
		return
	}

	// Install the Helm chart with DryRun to capture templated resources
	// Set namespace to composition's namespace for proper resource isolation
	installCfg := &helmconfig.InstallConfig{
		ActionConfig: &helmconfig.ActionConfig{
			ChartVersion:          compositionDefinition.Spec.Chart.Version,
			ChartName:             compositionDefinition.Spec.Chart.Repo,
			Values:                bValuesMap,
			Username:              "",
			Password:              "",
			InsecureSkipTLSverify: compositionDefinition.Spec.Chart.InsecureSkipVerifyTLS,
			DryRun:                helmconfig.DryRunServer,
			// This endpoint only RENDERS the chart (dry-run) to return its resources. DryRunServer is
			// required for template `lookup` (the installer umbrella gates child compositions on
			// cluster state), but an install-with-lookup also runs helm's adoption check and aborts
			// the ENTIRE render with a 500 if a single existing resource in the manifest carries
			// non-Helm ownership metadata ("cannot be imported into the current release: invalid
			// ownership metadata"). That makes one out-of-band/edited child composition wedge the whole
			// platform reconcile (D1, 2026-07-08). TakeOwnership skips that adoption abort while
			// keeping server-side lookup; it is inert here because DryRun performs no real apply.
			TakeOwnership: true,
			IncludeCRDs:   true,
			SkipCRDs:      false,
		},
		Namespace:       compositionNamespace, // Override namespace for this composition
		CreateNamespace: true,
		RestConfig:      wrappedCfg, // Pass wrapped config with tracer integration
	}

	// Retrieve credentials from secret if specified
	if compositionDefinition.Spec.Chart != nil && compositionDefinition.Spec.Chart.Credentials != nil {
		installCfg.ActionConfig.Username = compositionDefinition.Spec.Chart.Credentials.Username

		// Retrieve password from secret
		passwd, err := k8scli.GetSecret(compositionDefinition.Spec.Chart.Credentials.PasswordRef)
		if err != nil {
			log.ErrorContext(ctx, "unable to get secret",
				slog.Any("err", err),
			)
			response.InternalError(w, err)
			return
		}
		installCfg.ActionConfig.Password = passwd
	}

	// Install with DryRun to get templated manifest using global helm client with tracer integration
	// releaseName from the QUERY, never compositionMeta.GetReleaseName(composition). See the
	// validation above: the label on the live object is not the caller's resolved name on every
	// path, and reading it here produced a permanent retry loop (#329).
	op, err := h.HelmClient.Install(ctx, releaseName, compositionDefinition.Spec.Chart.Url, installCfg)
	var rel *helmconfig.Release
	if op != nil {
		rel = op.Release // plumbing v1.50.0: Install returns an OperationResult
	}
	if err != nil {
		log.ErrorContext(ctx, "unable to template chart",
			slog.Any("err", err),
		)
		// Answered with the code that says WHOSE fault this is; see installFailureStatus for the
		// three classes and why each gets the code it does (#265, #325).
		code := installFailureStatus(err)
		switch code {
		case http.StatusBadRequest:
			err = withKrateoGlobalNote(err)
			log.ErrorContext(ctx, "chart values do not satisfy the chart's values.schema.json",
				slog.Any("err", err),
			)
		case http.StatusUnprocessableEntity:
			log.ErrorContext(ctx, "chart references a kind this cluster does not serve",
				slog.Any("err", err),
			)
		}
		st := response.New(code, err)
		if code == http.StatusBadRequest {
			st.Reason = ReasonValuesSchemaViolation
		}
		response.Encode(w, st)
		return
	}

	// Getting the resources
	resLi := tracer.GetResources()

	// Ensure resLi is not nil to avoid null in JSON response
	if resLi == nil {
		resLi = []resources.Resource{}
	}

	// Separate what the chart OWNS from what it merely LOOKS UP (#228). The dry-run above returns
	// the rendered manifest, which is the evidence the tracer lacks: a named observation of an
	// object the chart does not render is a read-only lookup, not a create existence-check, and
	// does not need the management superset. See ownership.go for why narrowing the verbs instead
	// would break teardown.
	//
	// Failure here never widens a grant and never fails the request: with no usable manifest
	// nothing is narrowed and the response is exactly what it was before.
	if rel != nil {
		rendered, rerr := renderedObjects(rel.GetManifest(), composition.GetNamespace(), h.Plurarizer)
		if rerr != nil {
			log.WarnContext(ctx, "could not read the rendered manifest; leaving observed verbs unnarrowed",
				slog.Any("err", rerr))
		} else {
			// LIFECYCLE HOOKS, which neither half of the evidence above can see (core-provider#311).
			//
			// The tracer records a DRY-RUN install, and a dry run executes no hooks. helm then keeps
			// hooks OUT of Manifest by construction, so renderedObjects cannot see them either. The
			// result was that no grant was ever generated for a hook object — and a `pre-delete`
			// hook turned teardown into an uninstall that could never succeed, because helm must
			// DELETE the previous hook object under before-hook-creation.
			//
			// Added BEFORE narrowLookupVerbs, and recorded in rendered.owned, which is the whole
			// trick: that function narrows any superset entry whose key it cannot find in the owned
			// set, and a hook is absent from Manifest by definition. Appending the entries without
			// the keys would see them immediately demoted to get/list/watch — reintroducing the
			// exact bug with extra steps.
			hookRes, hookKeys, hookUnresolved := hookObjects(rel.GetHooks(), composition.GetNamespace(), h.Plurarizer)
			for _, k := range hookKeys {
				rendered.owned[k] = struct{}{}
			}
			resLi = append(resLi, hookRes...)

			// A hook whose kind we could not resolve is REPORTED at install time rather than
			// discovered at teardown, and its group is marked opaque so nothing in it is narrowed
			// on evidence we do not have. This is the loud install-time failure that the rejected
			// alternative — refusing any chart with uncoverable hooks — was reaching for, without
			// rejecting the charts that work today because an ordinary resource happens to need
			// the same kind.
			if len(hookUnresolved) > 0 {
				for g := range hookGroupsOf(rel.GetHooks()) {
					rendered.opaqueGroups[g] = struct{}{}
				}
				log.WarnContext(ctx, "could not derive RBAC for every lifecycle hook; a teardown may fail",
					slog.Any("hooks", hookUnresolved), slog.Int("hooksCovered", len(hookRes)))
			} else if len(hookRes) > 0 {
				log.InfoContext(ctx, "granted RBAC for lifecycle hook objects",
					slog.Int("hookObjects", len(hookRes)))
			}

			var narrowed int
			resLi, narrowed = narrowLookupVerbs(resLi, rendered)
			if narrowed > 0 {
				log.InfoContext(ctx, "narrowed read-only named lookups to get/list/watch",
					slog.Int("narrowed", narrowed), slog.Int("observed", len(resLi)),
					slog.Int("rendered", len(rendered.owned)),
					slog.Int("opaqueGroups", len(rendered.opaqueGroups)))
			}
		}
	}

	if meta.IsVerbose(composition) {
		b, err := json.Marshal(resLi)
		if err != nil {
			log.ErrorContext(ctx, "unable to marshal resources for logging",
				slog.Any("err", err),
			)
		} else {
			log.DebugContext(ctx, "Retrieved resources",
				slog.String("resources", string(b)),
			)
		}
	}

	// write the response in JSON format
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	err = enc.Encode(resLi)
	if err != nil {
		log.ErrorContext(ctx, "unable to marshal resources",
			slog.Any("err", err),
		)
		response.InternalError(w, err)
		return
	}

	log.InfoContext(ctx, "Successfully handled request to get resources")
}

// ReasonValuesSchemaViolation marks the values-schema 400, so a caller can tell it from the other
// 400s this handler answers (missing query parameters, a missing releaseName) by what the response
// SAYS rather than by ruling the others out. plumbing documents Status.reason as clarifying the HTTP
// code without overriding it, which is exactly this use.
//
// A CONTRACT WITH THE CDC. go/composition-dynamic-controller/internal/chartinspector reads this exact
// value; the two modules cannot share a declaration, so each pins the literal in a test.
const ReasonValuesSchemaViolation response.StatusReason = "ValuesSchemaViolation"

// installFailureStatus maps an Install failure to the HTTP status that tells the caller WHOSE fault
// it is. A 5xx is a claim about THIS SERVER: it is the one class a caller is entitled to read as
// "not my fault, retry later", so answering it for a caller's mistake sends the caller to this
// service's logs and into a retry that cannot work.
//
// 400 — the chart's values do not satisfy its values.schema.json (core-provider#325).
//
//	Classified with errors.Is on plumbing's helmconfig.ErrValuesSchema (plumbing v1.25.0), never on
//	helm's text: helm has a typed error and flattens it to a string before an install returns
//	(chartutil/jsonschema.go:107, values.go:168), so plumbing validates first with helm's typed
//	entry point and wraps the failure in the sentinel. The canonical case is a subchart with
//	`additionalProperties: false`, which rejects the `global` key helm injects into every
//	dependency; plumbing's message names that case and says to remove the prohibition.
//
//	400 AND NOT 422, for two reasons that are each sufficient:
//	  - 422 already means something else here, and its consumer acts on that meaning. The cdc's
//	    chartRenderableFailure maps 422 to reason UnservedKind with a FIXED message, "install the
//	    missing CRD and this resolves without further action", and does not include the body. A
//	    schema violation answered 422 would tell the author to install a CRD and drop helm's
//	    explanation — the burying this issue is about, done by the consumer instead.
//	  - The two are different in time. 422 means "not satisfiable AS THINGS STAND" and must not be
//	    latched, because a CRD can arrive (see below). A schema violation is a property of the chart
//	    and the values the caller sent: nothing on the cluster changes it, and only the author can.
//	    That is a request the caller must change, which is what 400 says.
//	The message is err.Error() unchanged: the sentinel's sentence, then plumbing's description with
//	helm's own wording after "helm said:". It is the Status `message`, at the top of the body.
//
// 422 — the chart references a kind this cluster does not serve (#265).
//
//	A blueprint disagreeing with the cluster, not a fault in this service. 422 rather than 400: the
//	request is well-formed and unprocessable right now. IT MEANS "NOT SATISFIABLE NOW", NOT "NOT
//	SATISFIABLE", and a consumer must not latch on it. The detector cannot distinguish a kind that
//	will never exist from one whose CRD was created moments ago and has not reached this process's
//	RESTMapper — plumbing's own doc says so. What makes the classification sound HERE is the CRD
//	informer: this client is built WithCRDInformer, which Resets the mapper on CRD arrival, so a
//	miss that survives it is evidence of absence rather than of staleness.
//
// 500 — everything else: fetching the chart, building the client, reaching the API server.
//
// THE SCHEMA CHECK COMES FIRST, deliberately. The two classes are disjoint in production — plumbing
// returns ErrValuesSchema before the install resolves any kind — but IsRESTMappingMiss falls back to
// substring matching on the whole message, and helm's schema message quotes the caller's property
// names: a values key spelled "ensure CRDs are installed first" makes both fire (measured in
// TestHandler_SchemaViolationWhoseTextLooksLikeAnUnservedKindIs400Not422). A typed sentinel is the
// stronger evidence, so it decides.
func installFailureStatus(err error) int {
	switch {
	case errors.Is(err, helmconfig.ErrValuesSchema):
		return http.StatusBadRequest
	case helmv3.IsRESTMappingMiss(err):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// krateoGlobalNote is appended to every values-schema 400, because only this service knows it.
//
// plumbing's description of a rejected top-level `global` says "helm does NOT inject `global` at the
// top level, so either drop `global` from the values or remove `additionalProperties: false`". That
// is true of helm and false of a Krateo render: InjectGlobalValues, called above on the very values
// this install validates, ASSIGNS values["global"] (maps.SetNestedField replaces the key outright),
// and the cdc's real install does the same. So `global` is present on every composition whatever its
// spec says, and "drop it from the values" sends the author looking for a key their spec does not
// have — the misdirection #325 asks the 400 to prevent.
//
// UNCONDITIONAL, not gated on the message mentioning `global`. Gating it would mean matching helm's
// text, which this classification exists to avoid; and the note is a true statement about every
// render this service performs, so it is never wrong, only sometimes not the cause.
const krateoGlobalNote = "Note: Krateo adds a top-level `global` key (compositionName, compositionId " +
	"and other composition metadata) to every composition's values before rendering, replacing any " +
	"`global` in the composition's spec, so it is always present and cannot be dropped from the " +
	"values. A top-level values.schema.json with `additionalProperties: false` must declare `global` " +
	"under `properties`, or drop the prohibition."

// withKrateoGlobalNote keeps err in the chain, so errors.Is still finds the sentinel, and keeps its
// text first and unchanged; the note follows.
func withKrateoGlobalNote(err error) error {
	return fmt.Errorf("%w. %s", err, krateoGlobalNote)
}
