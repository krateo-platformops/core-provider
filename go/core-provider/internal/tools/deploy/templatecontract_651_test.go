package deploy

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"text/template"
	"text/template/parse"

	"github.com/Masterminds/sprig/v3"
	definitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/objects/templates"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// templateKeys are the keys a template uses, whichever branch is taken. It fails on anything that would
// make a key invisible to it: a dot rebound by range or with, a nested template, a field of a field.
func templateKeys(t *testing.T, what, body string) map[string]bool {
	t.Helper()
	tpl, err := template.New(what).Funcs(sprig.FuncMap()).Parse(body)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	keys := map[string]bool{}
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch x := n.(type) {
		case *parse.ListNode:
			if x != nil {
				for _, c := range x.Nodes {
					walk(c)
				}
			}
		case *parse.ActionNode:
			walk(x.Pipe)
		case *parse.IfNode:
			walk(x.Pipe)
			walk(x.List)
			if x.ElseList != nil {
				walk(x.ElseList)
			}
		case *parse.PipeNode:
			if len(x.Decl) != 0 {
				t.Fatalf("%s: declares a variable (%s): teach this test about it", what, x)
			}
			for _, c := range x.Cmds {
				for _, a := range c.Args {
					walk(a)
				}
			}
		case *parse.FieldNode:
			if len(x.Ident) != 1 {
				t.Fatalf("%s: %s is a field of a field: teach this test about it", what, x)
			}
			keys[x.Ident[0]] = true
		case *parse.TextNode, *parse.IdentifierNode, *parse.StringNode, *parse.NumberNode, *parse.BoolNode, *parse.CommentNode:
		default:
			t.Fatalf("%s: %T (%s): teach this test about it", what, n, n)
		}
	}
	walk(tpl.Root)
	return keys
}

func baseKeys() map[string]any {
	return templates.Values(templates.Renderoptions{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "pages", Namespace: "krateo-system", Name: "pages-v1-0-0"})
}

// tunedController sets every per-Kind setting a definition can.
func tunedController() *definitionsv1alpha1.ControllerConfig {
	workers, resync := int32(4), "2m"
	return &definitionsv1alpha1.ControllerConfig{Workers: &workers, ResyncInterval: &resync, Resources: &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}}}
}

// templateCallSites is every template file the chart ships and every set of values core-provider renders
// it with, through the functions production uses (templatevalues.go, cdcvalues.go).
func templateCallSites(t *testing.T) map[string]map[string][]any {
	t.Helper()
	depValues := func(o DeployOptions) []any {
		v, err := cdcDeploymentValues("sa", o, map[string]string{"A": "b"})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	none := map[string][]any{"every caller": nil}
	binding := map[string][]any{"Deploy, Lookup and Undeploy": bindingValues("sa", "ns")}
	pool := PoolOptions{Namespace: "ns", Assets: PoolAssets{AuthnURL: "http://authn"}}
	return map[string]map[string][]any{
		"serviceaccount.yaml":                      none,
		"clusterrole.yaml":                         none,
		"compositiondefinition-role.yaml":          none,
		"lease-role.yaml":                          none,
		"service.yaml":                             none,
		"clusterrolebinding.yaml":                  binding,
		"compositiondefinition-rolebinding.yaml":   binding,
		"secret-rolebinding.yaml":                  binding,
		"lease-rolebinding.yaml":                   binding,
		"secret-role.yaml":                         {"Deploy, Lookup and Undeploy": secretRoleValues("secret")},
		"jsonschema-configmap.yaml/configmap.yaml": {"every caller": schemaConfigMapValues("{}")},
		"authn-serviceaccount.yaml": {
			"a controller's":  authnMapping{saName: "sa", saNamespace: "ns", group: "g", displayName: "d", principal: "p", delegation: "true", delegationAllowed: "true", delegationMaxDepth: "2"}.values(),
			"core-provider's": authnMapping{saName: "sa", saNamespace: "ns", group: "g", displayName: "d"}.values(),
		},
		"cdc-configmap.yaml/configmap.yaml": {
			"dedicated":            envConfigMapValues("sa", "ns", DeployOptions{}),
			"dedicated, tuned":     envConfigMapValues("sa", "ns", DeployOptions{Controller: tunedController()}),
			"a pool member":        envConfigMapValues("sa", "ns", DeployOptions{Pool: "general"}),
			"a pool member, tuned": envConfigMapValues("sa", "ns", DeployOptions{Pool: "general", Controller: tunedController()}),
		},
		"deployment.yaml": {
			"Deploy":        depValues(DeployOptions{}),
			"Deploy, tuned": depValues(DeployOptions{Controller: tunedController(), ApiRefName: "api"}),
			"Lookup":        deploymentIdentityValues("sa", "api"),
			"Undeploy":      deploymentIdentityValues("sa", ""),
		},
		"pool-configmap.yaml":  {"EnsurePool": poolConfigMapValues(pool, "general", PoolSizing{Workers: 4})},
		"pool-deployment.yaml": {"EnsurePool": poolDeploymentValues(pool, "general", `{"limits":{}}`, map[string]string{"A": "b"})},
	}
}

// chartValueSets are chart values that change what the templates contain: the chart renders each template
// before core-provider does.
var chartValueSets = map[string]map[string]any{
	"defaults": nil,
	"everything on": {
		"otel": map[string]any{"enabled": true},
		"cdc":  map[string]any{"metrics": map[string]any{"enabled": true}, "workers": 3, "resyncInterval": "5m"},
	},
}

// TestEveryTemplateKeyIsDeclared: each key a shipped template uses, in any branch, is passed by every call
// site that renders it; and each call site renders with nothing left as "<no value>". A template file the
// chart ships and no call site renders, or the reverse, fails.
func TestEveryTemplateKeyIsDeclared(t *testing.T) {
	sites := templateCallSites(t)
	for set, values := range chartValueSets {
		t.Run(set, func(t *testing.T) {
			rendered := map[string]bool{}
			n := 0
			for asset, cm := range templateConfigMaps(t, renderShippedChart(t, values)) {
				for file, body := range cm.Data {
					if file == "pool-sizing.json" { // not a template: cdc.pool as JSON (LoadPoolSizing)
						continue
					}
					id := file
					if _, ok := sites[id]; !ok {
						id = templateAssets[asset] + "/" + file
					}
					if len(sites[id]) == 0 {
						t.Errorf("%s/%s: no call site declares the values it is rendered with", asset, file)
						continue
					}
					rendered[id] = true
					used := templateKeys(t, id, body)
					for site, extra := range sites[id] {
						vals := baseKeys()
						for i := 0; i < len(extra); i += 2 {
							vals[extra[i].(string)] = extra[i+1]
						}
						for _, k := range slices.Sorted(maps.Keys(used)) {
							if v, ok := vals[k]; !ok || v == nil {
								t.Errorf("%s uses .%s, which %q does not pass", id, k, site)
							}
						}
						out, err := templates.Template(body).Render(vals)
						if err != nil {
							t.Errorf("%s, %q: %v", id, site, err)
						}
						if strings.Contains(string(out), "<no value>") {
							t.Errorf("%s, %q renders <no value>", id, site)
						}
						n++
					}
				}
			}
			for id := range sites {
				if !rendered[id] {
					t.Errorf("call site for %s: the chart ships no such template", id)
				}
			}
			t.Logf("%d templates, %d call sites", len(rendered), n)
		})
	}
}

// The render itself: an absent key is refused in every position, and "" is unset in every position. The
// quoted one is why a scan of the output for "<no value>" would not do: text/template hands quote a nil
// and it prints nothing, which is how a URL_AUTHN of "" would reach a controller.
func TestRenderRefusesAMissingKey(t *testing.T) {
	for body, unsetOut := range map[string]string{
		`[{{ .x }}]`:                     `[]`,
		`[{{ .x | quote }}]`:             `[""]`,
		`[{{ .x | default "d" }}]`:       `[d]`,
		`[{{ if .x }}a{{ end }}]`:        `[]`,
		`[{{ trunc 63 .x }}]`:            `[]`,
		`[{{ if .y }}{{ .x }}{{ end }}]`: `[]`,
	} {
		vals := map[string]any{"y": "on"}
		if out, err := templates.Template(body).Render(vals); err == nil {
			t.Errorf("%s without x rendered %q", body, out)
		} else if strings.Contains(body, "{{ .x }}") && !strings.Contains(err.Error(), `"x"`) {
			t.Errorf("%s: the error does not name the key: %v", body, err)
		}
		vals["x"] = ""
		out, err := templates.Template(body).Render(vals)
		if err != nil || string(out) != unsetOut {
			t.Errorf("%s with x unset = %q, %v; want %q", body, out, err, unsetOut)
		}
	}
}

// mountShipped writes the shipped templates where core-provider's paths point, as the kubelet does.
func mountShipped(t *testing.T, edit func(file, body string) string) (root string) {
	t.Helper()
	root = t.TempDir()
	dirs := map[string]string{"cdc-deployment": "cdc-deployment", "cdc-configmap": "cdc-configmap", "rbac-configmap": "cdc-rbac",
		"jsonschema-configmap": "json-schema-configmap", "service-configmap": "cdc-service"}
	for asset, cm := range templateConfigMaps(t, renderShippedChart(t, nil)) {
		dir := filepath.Join(root, dirs[asset])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range cm.Data {
			if edit != nil {
				v = edit(dirs[asset]+"/"+k, v)
			}
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func contractCluster(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		portalsGVR: "PortalList", compositionDefinitionGVR: "CompositionDefinitionList", configMapGVR: "ConfigMapList",
		clusterRoleGVR: "ClusterRoleList", clusterRoleBindingGVR: "ClusterRoleBindingList", roleGVR: "RoleList", roleBindingGVR: "RoleBindingList",
		serviceAccountGVR: "ServiceAccountList", deploymentGVR: "DeploymentList", serviceGVR: "ServiceList",
	}, objs...)
}

func credentialsSpec() *definitionsv1alpha1.ChartInfo {
	c := &definitionsv1alpha1.ChartInfo{Url: "oci://example/portal", Version: "1.8.127", Credentials: &definitionsv1alpha1.Credentials{Username: "u"}}
	c.Credentials.PasswordRef.Namespace, c.Credentials.PasswordRef.Name, c.Credentials.PasswordRef.Key = "creds", "chart-creds", "password"
	return c
}

func contractDeployOptions(root string, dyn *dynamicfake.FakeDynamicClient) DeployOptions {
	return DeployOptions{
		GVR: portalsGVR, Namespace: "tenant-a", ControllerNamespace: "krateo-system", Spec: credentialsSpec(), DynClient: dyn,
		KubeClient:             clientfake.NewClientBuilder().Build(),
		RBACFolderPath:         filepath.Join(root, "cdc-rbac"),
		DeploymentTemplatePath: filepath.Join(root, "cdc-deployment/deployment.yaml"),
		ConfigmapTemplatePath:  filepath.Join(root, "cdc-configmap/configmap.yaml"),
		JsonSchemaTemplatePath: filepath.Join(root, "json-schema-configmap/configmap.yaml"),
		ServiceTemplatePath:    filepath.Join(root, "cdc-service/service.yaml"),
		JsonSchemaBytes:        []byte(`{"type":"object"}`), ControllerAuthnURL: "http://authn",
		DryRunServer: true, // no wait for a rollout nothing performs here
	}
}

func contractUndeployOptions(root string, dyn *dynamicfake.FakeDynamicClient) UndeployOptions {
	o := contractDeployOptions(root, dyn)
	return UndeployOptions{
		GVR: o.GVR, Namespace: o.Namespace, ControllerNamespace: o.ControllerNamespace, Spec: o.Spec, DynamicClient: dyn, KubeClient: o.KubeClient,
		RBACFolderPath: o.RBACFolderPath, DeploymentTemplatePath: o.DeploymentTemplatePath, ConfigmapTemplatePath: o.ConfigmapTemplatePath,
		JsonSchemaTemplatePath: o.JsonSchemaTemplatePath, ServiceTemplatePath: o.ServiceTemplatePath, SkipCRD: true,
	}
}

func contractCtx() context.Context {
	return contexttools.CtxWithLogger(context.Background(), logging.NewNopLogger())
}

// The real callers with the shipped templates and chart credentials: a deploy, its lookup, and its
// teardown. Undeploy rendered the credentials RoleBinding without its subject until #651.
func TestShippedTemplatesThroughDeployLookupUndeploy(t *testing.T) {
	root := mountShipped(t, nil)
	dyn := contractCluster()
	ctx := contractCtx()
	if _, err := Deploy(ctx, nil, contractDeployOptions(root, dyn)); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for _, o := range []struct {
		gvr      schema.GroupVersionResource
		ns, name string
	}{
		{deploymentGVR, "krateo-system", "portals-v1-8-127-controller"},
		{configMapGVR, "krateo-system", "portals-v1-8-127-configmap"},
		{serviceGVR, "krateo-system", "portals-v1-8-127-controller-service"},
		{roleGVR, "krateo-system", "portals-v1-8-127-lease"},
		{roleBindingGVR, "creds", "portals-v1-8-127-secret"},
	} {
		u, err := dyn.Resource(o.gvr).Namespace(o.ns).Get(ctx, o.name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("%s %s/%s: %v", o.gvr.Resource, o.ns, o.name, err)
		}
		if b, _ := u.MarshalJSON(); strings.Contains(string(b), "<no value>") {
			t.Errorf("%s %s carries <no value>: %s", o.gvr.Resource, o.name, b)
		}
	}
	if _, err := Lookup(ctx, nil, contractDeployOptions(root, dyn)); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := Undeploy(ctx, nil, contractUndeployOptions(root, dyn)); err != nil {
		t.Fatalf("Undeploy: %v", err)
	}
	for _, gone := range []struct {
		gvr      schema.GroupVersionResource
		ns, name string
	}{{roleBindingGVR, "creds", "portals-v1-8-127-secret"}, {serviceGVR, "krateo-system", "portals-v1-8-127-controller-service"}} {
		if keepSharedExists(t, dyn, gone.gvr, gone.ns, gone.name) {
			t.Errorf("Undeploy left %s %s/%s behind", gone.gvr.Resource, gone.ns, gone.name)
		}
	}
	// The bindings' subject, as rendered (the key Undeploy did not pass).
	var authn = SelfAuthnMapping{SAName: "core-provider", SANamespace: "krateo-system", RBACFolderPath: filepath.Join(root, "cdc-rbac")}
	u, err := authn.render()
	if err != nil {
		t.Fatalf("core-provider's own authn mapping: %v", err)
	}
	if spec, _, _ := unstructured.NestedMap(u.Object, "spec"); spec["principal"] != nil || spec["delegation"] != nil {
		t.Errorf("core-provider's own mapping renders principal or delegation: %v", spec)
	}
}

// A template key core-provider does not know: the object is not rendered, so it is never applied, and the
// error names the file and the key. Each template in turn. (2.13.57 wrote a Lease named "<no value>".)
func TestAnUnknownTemplateKeyIsRefused(t *testing.T) {
	for _, file := range []string{
		"cdc-rbac/serviceaccount.yaml", "cdc-rbac/clusterrole.yaml", "cdc-rbac/lease-role.yaml", "cdc-rbac/secret-role.yaml",
		"cdc-configmap/configmap.yaml", "cdc-deployment/deployment.yaml", "cdc-service/service.yaml",
	} {
		t.Run(file, func(t *testing.T) {
			root := mountShipped(t, func(f, body string) string {
				if f == file {
					return body + "\n# {{ .aKeyFromANewerChart | quote }}\n"
				}
				return body
			})
			dyn := contractCluster()
			_, err := Deploy(contractCtx(), nil, contractDeployOptions(root, dyn))
			if err == nil || !strings.Contains(err.Error(), "aKeyFromANewerChart") || !strings.Contains(err.Error(), file) {
				t.Fatalf("err = %v, want one naming the key and %s", err, file)
			}
			if strings.HasPrefix(file, "cdc-configmap/") || strings.HasPrefix(file, "cdc-rbac/") {
				if keepSharedExists(t, dyn, deploymentGVR, "krateo-system", "portals-v1-8-127-controller") {
					t.Error("the controller Deployment was applied after the refusal")
				}
			}
		})
	}
}

// The Service template's mount is optional (cdc.metrics), so a missing file means no Service. Any other
// failure to read it is an error on every path: read as "no Service", Deploy dropped the Service from the
// digest and Undeploy left it behind.
func TestAnUnreadableServiceTemplateIsNotAnAbsentOne(t *testing.T) {
	root := mountShipped(t, nil)
	if ok, err := serviceTemplateMounted(filepath.Join(root, "cdc-service/service.yaml")); !ok || err != nil {
		t.Errorf("mounted: %v, %v", ok, err)
	}
	for _, p := range []string{"", filepath.Join(root, "no-such-dir/service.yaml")} {
		if ok, err := serviceTemplateMounted(p); ok || err != nil {
			t.Errorf("not mounted (%q): %v, %v; want absent and no error", p, ok, err)
		}
	}
	// A path THROUGH a file: ENOTDIR, which is neither present nor NotExist.
	bad := filepath.Join(root, "cdc-service/service.yaml/service.yaml")
	if _, err := os.Stat(bad); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("control: %v, want ENOTDIR", err)
	}
	dyn := contractCluster()
	ctx := contractCtx()
	good := contractDeployOptions(root, dyn)
	if _, err := Deploy(ctx, nil, good); err != nil {
		t.Fatal(err)
	}
	dopts, uopts := good, contractUndeployOptions(root, dyn)
	dopts.ServiceTemplatePath, uopts.ServiceTemplatePath = bad, bad
	for name, f := range map[string]func() error{
		"serviceTemplateMounted": func() error { _, err := serviceTemplateMounted(bad); return err },
		"Deploy":                 func() error { _, err := Deploy(ctx, nil, dopts); return err },
		"Lookup":                 func() error { _, err := Lookup(ctx, nil, dopts); return err },
		"DerivedNameViolations":  func() error { _, err := DerivedNameViolations(portalsGVR, true, bad); return err },
		"Undeploy":               func() error { return Undeploy(ctx, nil, uopts) },
	} {
		if err := f(); !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("%s: err = %v, want the read error", name, err)
		}
	}
	if !keepSharedExists(t, dyn, serviceGVR, "krateo-system", "portals-v1-8-127-controller-service") {
		t.Error("the Service is gone")
	}
	// And absent still means no Service, on the same cluster.
	none := good
	none.ServiceTemplatePath = filepath.Join(root, "no-such-dir/service.yaml")
	if _, err := Lookup(ctx, nil, none); err != nil {
		t.Errorf("Lookup without a Service template: %v", err)
	}
}
