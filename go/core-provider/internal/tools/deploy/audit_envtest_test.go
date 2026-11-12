//go:build envtest

package deploy

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#423, against a REAL apiserver, through deploy's own RBAC install and uninstall (the
// kube wrapper, objectclient, the apiserver's no-op Update): the first install records one create per
// object; a second, identical install records nothing (every Update short-circuited, resourceVersion
// unmoved); a rule change records one update, of the Role alone; the uninstall records one delete per
// object. Every successful row is checked against the cluster.
func TestAuditRecordsDeployRBACEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx0 := context.Background()
	ns := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": "audit-423"}}}
	if _, err := dyn.Resource(namespaceGVR).Create(ctx0, ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	sink := audittest.NewRecorder(t)
	e := audit.New(sink.Logger(auditrec.Scope), audit.WithWriter(audit.WriterController),
		audit.WithActor("system:serviceaccount:krateo-system:core-provider", audit.ClassController))
	r, err := auditrec.NewRecorder(e, metricnoop.NewMeterProvider(), auditrec.LabelsFrom(dyn))
	if err != nil {
		t.Fatal(err)
	}
	cause := audit.CauseRef{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions",
		Namespace: "audit-423", Name: "fireworksapp", UID: "00000000-0000-4000-8000-0000000000cd", Generation: 1}
	ctx := auditrec.WithScope(ctx0, r, cause)

	rules := func(verbs ...string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: verbs}}
	}
	objs := func(roleVerbs ...string) (rbacv1.ClusterRole, rbacv1.ClusterRoleBinding, rbacv1.Role, rbacv1.RoleBinding, corev1.ServiceAccount) {
		subj := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "fireworksapps-v1-0-0-controller", Namespace: "audit-423"}}
		return rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0"}, Rules: rules("get")},
			rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0"}, Subjects: subj,
				RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "fireworksapps-v1-0-0"}},
			rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0", Namespace: "audit-423"}, Rules: rules(roleVerbs...)},
			rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0", Namespace: "audit-423"}, Subjects: subj,
				RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "fireworksapps-v1-0-0"}},
			corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0-controller", Namespace: "audit-423"}}
	}
	install := func(roleVerbs ...string) {
		t.Helper()
		cr, crb, role, rb, sa := objs(roleVerbs...)
		hsh := hasher.NewFNVObjectHash()
		if err := installRBACResources(ctx, dyn, cr, crb, role, rb, sa, &hsh, kubecli.ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. First install: one create per object.
	install("get")
	rows := auditRows(t, sink)
	if got, want := names(rows), []string{"create ClusterRole", "create ClusterRoleBinding", "create Role", "create RoleBinding", "create ServiceAccount"}; !equal(got, want) {
		t.Fatalf("first install recorded %v, want %v", got, want)
	}
	checkAgainstCluster(ctx0, t, dyn, rows)

	// 2. The same install again: every Update is a no-op on the apiserver, so nothing is recorded.
	sink.Reset()
	install("get")
	if rows := auditRows(t, sink); len(rows) != 0 {
		t.Fatalf("an identical re-install recorded %v, want nothing", names(rows))
	}

	// 3. The Role's rules change: one update, of the Role only.
	sink.Reset()
	install("get", "list")
	rows = auditRows(t, sink)
	if got, want := names(rows), []string{"update Role"}; !equal(got, want) {
		t.Fatalf("a rule change recorded %v, want %v", got, want)
	}
	checkAgainstCluster(ctx0, t, dyn, rows)

	// 4. Uninstall: one delete per object.
	sink.Reset()
	cr, crb, role, rb, sa := objs("get", "list")
	if err := uninstallRBACResources(ctx, dyn, cr, crb, role, rb, sa, false, false); err != nil {
		t.Fatal(err)
	}
	rows = auditRows(t, sink)
	if got, want := names(rows), []string{"delete ClusterRole", "delete ClusterRoleBinding", "delete Role", "delete RoleBinding", "delete ServiceAccount"}; !equal(got, want) {
		t.Fatalf("uninstall recorded %v, want %v", got, want)
	}
	checkAgainstCluster(ctx0, t, dyn, rows)
}

type row struct{ verb, kind, group, version, resource, ns, name, uid string }

func attrOf(rec sdklog.Record, key string) string {
	for _, kv := range audittest.Attributes(rec) {
		if string(kv.Key) == key && kv.Value.Type() == attribute.STRING {
			return kv.Value.AsString()
		}
	}
	return ""
}

func auditRows(t *testing.T, sink *audittest.Recorder) []row {
	t.Helper()
	var out []row
	for _, rec := range sink.Records() {
		audittest.AssertConformant(t, audit.WriterController, rec)
		if attrOf(rec, "krateo.action") != auditrec.ActionRBACApply && attrOf(rec, "krateo.action") != auditrec.ActionRBACDelete {
			t.Errorf("unexpected action %q", attrOf(rec, "krateo.action"))
		}
		if attrOf(rec, "krateo.audit.cause_ref.uid") != "00000000-0000-4000-8000-0000000000cd" || attrOf(rec, "krateo.audit.outcome") != audit.OutcomeSuccess {
			t.Errorf("record cause/outcome wrong: %q %q", attrOf(rec, "krateo.audit.cause_ref.uid"), attrOf(rec, "krateo.audit.outcome"))
		}
		out = append(out, row{
			verb: attrOf(rec, "krateo.audit.verb"), kind: attrOf(rec, "krateo.audit.object_ref.kind"),
			group: attrOf(rec, "krateo.audit.object_ref.api_group"), version: attrOf(rec, "krateo.audit.object_ref.api_version"),
			resource: attrOf(rec, "krateo.audit.object_ref.resource"), ns: attrOf(rec, "k8s.namespace.name"),
			name: attrOf(rec, "krateo.audit.object_ref.name"), uid: attrOf(rec, "krateo.audit.object_ref.uid"),
		})
	}
	return out
}

func names(rows []row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.verb+" "+r.kind)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// checkAgainstCluster: a create or update row names an object that exists with the recorded uid; a
// delete row names one that is gone.
func checkAgainstCluster(ctx context.Context, t *testing.T, dyn dynamic.Interface, rows []row) {
	t.Helper()
	for _, r := range rows {
		ri := dyn.Resource(schema.GroupVersionResource{Group: r.group, Version: r.version, Resource: r.resource})
		var err error
		var uid string
		if r.ns != "" {
			o, gerr := ri.Namespace(r.ns).Get(ctx, r.name, metav1.GetOptions{})
			err = gerr
			if o != nil {
				uid = string(o.GetUID())
			}
		} else {
			o, gerr := ri.Get(ctx, r.name, metav1.GetOptions{})
			err = gerr
			if o != nil {
				uid = string(o.GetUID())
			}
		}
		switch r.verb {
		case "delete":
			if err == nil {
				t.Errorf("%s %s/%s recorded deleted, but it is live", r.kind, r.ns, r.name)
			} else if !apierrors.IsNotFound(err) {
				t.Errorf("%s %s/%s: reading it back: %v", r.kind, r.ns, r.name, err)
			}
		default:
			if err != nil {
				t.Errorf("%s %s/%s recorded %s, but cannot be read: %v", r.kind, r.ns, r.name, r.verb, err)
			} else if uid != r.uid {
				t.Errorf("%s %s/%s records uid %q, live %q", r.kind, r.ns, r.name, r.uid, uid)
			}
		}
	}
}
