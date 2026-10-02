package resources

import (
	"encoding/json"
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
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmutils "github.com/krateo-platformops/plumbing/helm/utils"
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
	_, err = h.HelmClient.Install(ctx, compositionMeta.GetReleaseName(composition), compositionDefinition.Spec.Chart.Url, installCfg)
	if err != nil {
		log.ErrorContext(ctx, "unable to template chart",
			slog.Any("err", err),
		)
		response.InternalError(w, err)
		return
	}

	// Getting the resources
	resLi := tracer.GetResources()

	// Ensure resLi is not nil to avoid null in JSON response
	if resLi == nil {
		resLi = []resources.Resource{}
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
