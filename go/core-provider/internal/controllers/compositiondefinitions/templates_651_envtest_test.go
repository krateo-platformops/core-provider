//go:build envtest

package compositiondefinitions

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// core-provider#651 on a real apiserver: a mounted template that uses a key this core-provider does not
// pass (a chart and an image that do not belong together) is refused before anything rendered from it is
// applied, on the rows that already say so, and the definition recovers by itself. It used to be applied
// with "<no value>".
func TestEnvtest_651_UnknownTemplateKeyIsRefused(t *testing.T) {
	e := start644(t)
	ctx := context.Background()
	d := e.settled(t, "unknownkeys")

	var deps appsv1.DeploymentList
	if err := e.c.cl.List(ctx, &deps, client.InNamespace(self642NS)); err != nil {
		t.Fatal(err)
	}
	if len(deps.Items) != 1 {
		t.Fatalf("%d controller Deployments, want 1", len(deps.Items))
	}
	stem := strings.TrimSuffix(deps.Items[0].Name, "-controller")

	versions := func() string {
		t.Helper()
		var out []string
		for _, o := range []struct {
			obj  client.Object
			name string
		}{
			{&appsv1.Deployment{}, stem + "-controller"}, {&corev1.ConfigMap{}, stem + "-configmap"},
			{&corev1.ServiceAccount{}, stem}, {&rbacv1.Role{}, stem + "-lease"},
		} {
			if err := e.c.cl.Get(ctx, types.NamespacedName{Namespace: self642NS, Name: o.name}, o.obj); err != nil {
				t.Fatalf("%T %s: %v", o.obj, o.name, err)
			}
			if _, ok := o.obj.(*appsv1.Deployment); ok { // its status moves
				out = append(out, o.name, string(o.obj.GetUID()), strconv.FormatInt(o.obj.GetGeneration(), 10))
				continue
			}
			out = append(out, o.name, o.obj.GetResourceVersion())
		}
		return strings.Join(out, " ")
	}
	before := versions()

	for _, path := range []string{CDCtemplateConfigmapPath, CDCtemplateDeploymentPath} {
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Quoted: rendered as before, it would print nothing at all, not even "<no value>".
		if err := os.WriteFile(path, append(orig, []byte("\n# {{ .aKeyFromANewerChart | quote }}\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := d.pass(); err == nil {
				t.Errorf("%s: a pass succeeded with an unknown key", path)
			}
		}
		if d.cond("Ready") != "Unknown/NotObserved" || d.cond("Synced") != "False/ReconcileError" {
			t.Errorf("%s: Ready=%s Synced=%s, want Unknown/NotObserved and False/ReconcileError", path, d.cond("Ready"), d.cond("Synced"))
		}
		if m := d.message("Synced"); !strings.Contains(m, "aKeyFromANewerChart") || !strings.Contains(m, path) {
			t.Errorf("%s: Synced does not name the key and the file: %q", path, m)
		}
		if got := versions(); got != before {
			t.Errorf("%s: something was written:\n%s\nwant\n%s", path, got, before)
		}
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		d.rest(4)
		if got := versions(); got != before {
			t.Errorf("%s: recovering wrote something:\n%s\nwant\n%s", path, got, before)
		}
	}
}

// The teardown of a definition with chart credentials renders every object it deletes, the credentials
// RoleBinding included, whose subject it did not pass.
func TestEnvtest_651_TeardownWithCredentials(t *testing.T) {
	e := start644(t)
	ctx := context.Background()
	const creds = "chart-creds"
	if err := e.c.cl.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: self642CDNS, Name: creds}, Data: map[string][]byte{"password": []byte("p")}}); err != nil {
		t.Fatal(err)
	}
	e.charts.add(t, "withcreds", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "withcreds", "withcreds", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		c := &compositiondefinitionsv1alpha1.Credentials{Username: "u"}
		c.PasswordRef.Namespace, c.PasswordRef.Name, c.PasswordRef.Key = self642CDNS, creds, "password"
		cd.Spec.Chart.Credentials = c
	})
	d.rest(6)

	var rbs rbacv1.RoleBindingList
	count := func() (n int) {
		t.Helper()
		if err := e.c.cl.List(ctx, &rbs, client.InNamespace(self642CDNS)); err != nil {
			t.Fatal(err)
		}
		for _, rb := range rbs.Items {
			if strings.HasSuffix(rb.Name, "-secret") {
				n++
				if len(rb.Subjects) != 1 || rb.Subjects[0].Namespace != self642NS || rb.Subjects[0].Name == "" {
					t.Errorf("%s binds %+v", rb.Name, rb.Subjects)
				}
			}
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("%d credentials RoleBindings, want 1", count())
	}
	if err := e.c.cl.Delete(ctx, d.cd()); err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 6 && count() != 0; i++ {
		_, last = d.pass()
	}
	if count() != 0 {
		t.Errorf("the credentials RoleBinding was left behind; last pass: %v", last)
	}
	if n := countWithSuffix(t, e, "-controller"); n != 0 {
		t.Errorf("%d controller Deployments left; last pass: %v", n, last)
	}
}

func countWithSuffix(t *testing.T, e *env644, suffix string) (n int) {
	t.Helper()
	var deps appsv1.DeploymentList
	if err := e.c.cl.List(context.Background(), &deps, client.InNamespace(self642NS)); err != nil {
		t.Fatal(err)
	}
	for _, d := range deps.Items {
		if strings.HasSuffix(d.Name, suffix) {
			n++
		}
	}
	return n
}
