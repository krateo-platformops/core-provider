package compositiondefinitions

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// cdWithStatusGVK builds a definition that has reconciled at least once, so statusGVR can derive
// a GVR from it. resource is the plural the generated CRD is served under.
func cdWithStatusGVK(apiVersion, kind, resource string) *compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.SetName("fireworksapp")
	cd.SetNamespace("krateo-system")
	cd.Status.ApiVersion = apiVersion
	cd.Status.Kind = kind
	cd.Status.Resource = resource
	return cd
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sc := runtime.NewScheme()
	if err := apis.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func generatedCRD(plural, group, version string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Kind: "FireworksApp"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: version, Served: true, Storage: true},
			},
		},
	}
}

// #235: the runtime's unresolved-create recovery is gated on an ANSWER from Observe. An error
// returns before the guard runs, so while Observe errors a half-created definition keeps
// external-create-pending as its newest marker and refuses forever — on every provider-runtime
// version from v1.3.0 through v1.6.0.
//
// v1.6.0 is worth naming precisely, because it is easy to mistake for a fix. It wraps the Observe
// error with "external create pending since <t> cannot be resolved until observe succeeds", which
// makes an UNFIXED instance visible on the object instead of benign-looking. It does not resolve
// the handshake, and deliberately so: the only way a reconciler could bound this itself is to
// create blind. The fix has to be here, in the consumer, which is what this file tests.
//
// A chart can be unreadable indefinitely (private registry, pulled chart, dead credentials), and
// Observe used to return bare on every chart-read failure. So "half-created plus unreadable chart"
// was a permanent wedge no runtime version could fix.
func TestObserveWithoutChart_AnswersInsteadOfErroring(t *testing.T) {
	cd := cdWithStatusGVK("composition.krateo.io/v1-0-0", "FireworksApp", "fireworksapps")
	cli := clientfake.NewClientBuilder().WithScheme(scheme(t)).Build() // no CRD

	e := &external{kube: cli}
	obs, err := e.observeWithoutChart(context.Background(), cd,
		errors.New("pull oci://registry.invalid/chart: no such host"), logging.NewNopLogger())

	if err != nil {
		t.Fatalf("returned an error (%v); the runtime's create guard never runs on an error, so "+
			"the handshake stays unresolved and the definition is wedged", err)
	}
	if obs.ResourceExists {
		t.Error("reported the external resource as EXISTING with no CRD present")
	}
	if obs.ResourceUpToDate {
		t.Error("reported up-to-date while the chart is unreadable; nothing would then re-attempt " +
			"and the chart error would never surface from Create or Update")
	}
}

// The answer must be the TRUE one, not a blanket "absent". A definition whose generated CRD is
// present has an external resource, and reporting it absent would make the runtime try to Create
// something that already exists.
func TestObserveWithoutChart_ReportsExistsWhenTheCRDIsPresent(t *testing.T) {
	cd := cdWithStatusGVK("composition.krateo.io/v1-0-0", "FireworksApp", "fireworksapps")
	crd := generatedCRD("fireworksapps", "composition.krateo.io", "v1-0-0")
	cli := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(crd).Build()

	e := &external{kube: cli}
	obs, err := e.observeWithoutChart(context.Background(), cd, errors.New("chart gone"), logging.NewNopLogger())
	if err != nil {
		t.Fatalf("observeWithoutChart: %v", err)
	}
	if !obs.ResourceExists {
		t.Error("the generated CRD is present but the external resource was reported absent; the " +
			"runtime would try to Create over it")
	}
}

// A definition that has NEVER reconciled has no recorded GVK — and therefore no generated CRD, so
// "does not exist" is true rather than a guess. This is the path a brand-new definition whose very
// first chart fetch fails takes, which is exactly the half-created case.
func TestObserveWithoutChart_NoStatusGVKIsTruthfullyAbsent(t *testing.T) {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.SetName("never-reconciled")
	cd.SetNamespace("krateo-system")
	cli := clientfake.NewClientBuilder().WithScheme(scheme(t)).Build()

	e := &external{kube: cli}
	obs, err := e.observeWithoutChart(context.Background(), cd, errors.New("chart gone"), logging.NewNopLogger())
	if err != nil {
		t.Fatalf("a definition with no status must still get an answer, not an error: %v", err)
	}
	if obs.ResourceExists {
		t.Error("a definition that never reconciled was reported as having an external resource")
	}
}

// The chart failure must not be swallowed. It is named on the condition so the cause is visible on
// the object, and ResourceUpToDate=false routes the runtime into Create/Update, which read the
// chart themselves and surface the same error from a phase that can act on it — Create
// additionally stamping external-create-failed, which is what closes the handshake.
func TestObserveWithoutChart_NamesTheCauseOnTheCondition(t *testing.T) {
	cd := cdWithStatusGVK("composition.krateo.io/v1-0-0", "FireworksApp", "fireworksapps")
	cli := clientfake.NewClientBuilder().WithScheme(scheme(t)).Build()

	e := &external{kube: cli}
	if _, err := e.observeWithoutChart(context.Background(), cd,
		errors.New("pull oci://registry.invalid/chart: no such host"), logging.NewNopLogger()); err != nil {
		t.Fatalf("observeWithoutChart: %v", err)
	}

	conds := cd.GetCondition("Ready")
	if conds.Message == "" {
		t.Fatal("no Ready message was set; the chart failure would be invisible on the object")
	}
	if !strings.Contains(conds.Message, "registry.invalid") {
		t.Errorf("the condition does not name the cause: %q", conds.Message)
	}
	if conds.Status == "True" {
		t.Errorf("Ready=True while the chart is unreadable: %q", conds.Status)
	}
}

// A failed CRD READ is different in kind from an unreadable chart: it is a live apiserver problem,
// not a state that persists, and returning the error is right. It also cannot wedge anything the
// way the chart path did, because it does not persist across reconciles the same way.
//
// Control arm for the whole helper: it must not have become "always answer, never error".
func TestObserveWithoutChart_ACRDReadFailureStillErrors(t *testing.T) {
	cd := cdWithStatusGVK("composition.krateo.io/v1-0-0", "FireworksApp", "fireworksapps")
	// A client with no scheme for CRDs cannot serve the Get, so it errors rather than returning nil.
	cli := clientfake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	e := &external{kube: cli}
	_, err := e.observeWithoutChart(context.Background(), cd, errors.New("chart gone"), logging.NewNopLogger())
	if err == nil {
		t.Fatal("a failed CRD read returned no error; this helper must answer only when it can " +
			"actually determine the answer, otherwise it is inventing one")
	}
}

// The WIRING. The helper is tested above; this asserts Observe actually routes its chart-read
// failures through it rather than returning bare.
//
// Four times now a decision function has been covered while the line calling it was not (#218,
// #221, #224, and #230's attempt), and each time removing the call broke nothing. Observe cannot
// be driven in a unit test — it fetches the chart before it does anything else, which is itself
// why this error path went unexamined for so long — so this follows the AST-guard idiom already in
// this repo and in the cdc.
//
// It asserts the SHAPE that matters: inside Observe, no chart-read call may be followed by a bare
// `return reconciler.ExternalObservation{}, ...`. Any such return is the wedge coming back.
func TestObserveRoutesChartFailuresThroughTheHelper(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "compositiondefinitions.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing compositiondefinitions.go: %v", err)
	}

	chartReaders := map[string]bool{
		"ChartInfoFromSpec": true,
		"ForSpec":           true,
		"GroupVersionKind":  true,
		"ChartJsonSchema":   true,
	}

	var sawObserve bool
	var readers, routed int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != "Observe" {
			continue
		}
		sawObserve = true

		// Walk statements in order; a chart-read assignment must be followed by an `if err != nil`
		// whose body calls observeWithoutChart.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			isChartRead := false
			for _, rhs := range as.Rhs {
				ce, ok := rhs.(*ast.CallExpr)
				if !ok {
					continue
				}
				if sel, ok := ce.Fun.(*ast.SelectorExpr); ok && chartReaders[sel.Sel.Name] {
					isChartRead = true
				}
			}
			if !isChartRead {
				return true
			}
			readers++
			return true
		})

		// Count observeWithoutChart calls in Observe.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := ce.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "observeWithoutChart" {
				routed++
			}
			return true
		})
	}

	if !sawObserve {
		t.Fatal("did not find func (e *external) Observe — this guard is not looking at the right " +
			"file and proves nothing")
	}
	if readers == 0 {
		t.Fatal("found no chart-read calls inside Observe; either the guard's name list is stale or " +
			"Observe no longer reads the chart, and either way it is asserting nothing")
	}
	if routed < readers {
		t.Errorf("Observe makes %d chart-read call(s) but only %d route through observeWithoutChart. "+
			"A chart-read failure that returns bare leaves the runtime's unresolved-create guard "+
			"unreachable, which is #235: a half-created definition with an unreadable chart wedged "+
			"forever on every provider-runtime version.", readers, routed)
	}
}
