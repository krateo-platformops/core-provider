//go:build envtest

package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#423, the FAILURE arm of the RBAC set (audit v0.14.0: every outcome a coverage row
// lists is checked by AssertConformant on records from the real path). A real apiserver, with RBAC
// authorization, refuses deploy's own install and uninstall, run as a user that holds less than the
// objects it writes:
//
//   - create refused: the ClusterRole grants configmaps get, which the writer does not hold, so RBAC
//     escalation prevention answers 403. One record: create / failure / 403, no object uid (nothing
//     was persisted), and the ClusterRole is absent.
//   - update refused: once the writer holds configmaps get, the install succeeds; a Role that adds
//     list (not held) is refused 403. One record: update / failure / 403 with the LIVE Role's uid, and
//     the live Role still has only get. The unchanged ClusterRole and ClusterRoleBinding before it
//     record nothing.
//   - delete refused: the writer has no delete on clusterroles. One record: delete / failure / 403
//     with the live uid, and the ClusterRole is still live.
//
// Every record is conformant, carries the CompositionDefinition as cause_ref, and is checked against
// the cluster read as admin (an independent source).
func TestAuditRecordsDeployRBACRefusalEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx0 := context.Background()
	admin := dynamic.NewForConfigOrDie(cfg)
	adminKube := kubernetes.NewForConfigOrDie(cfg)

	const nsName = "audit-423-refused"
	if _, err := adminKube.CoreV1().Namespaces().Create(ctx0, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The writer: may read and write the five kinds deploy installs, but holds no configmaps
	// permission and may not delete ClusterRoles. No escalate or bind verb.
	const writer = "core-provider-refused"
	user, err := env.AddUser(envtest.User{Name: writer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(user.Config())
	writerKube := kubernetes.NewForConfigOrDie(user.Config())
	grant(ctx0, t, adminKube, writer+"-base", writer, []rbacv1.PolicyRule{
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterroles"}, Verbs: []string{"get", "create", "update"}},
		{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterrolebindings", "roles", "rolebindings"}, Verbs: []string{"get", "create", "update", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get", "create", "update", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get"}},
	})
	waitAllowed(ctx0, t, writerKube, authorizationv1.ResourceAttributes{Group: "rbac.authorization.k8s.io", Resource: "clusterroles", Verb: "create"}, true)
	waitAllowed(ctx0, t, writerKube, authorizationv1.ResourceAttributes{Group: "", Resource: "configmaps", Verb: "get"}, false)

	sink := audittest.NewRecorder(t)
	e := audit.New(sink.Logger(auditrec.Scope), audit.WithWriter(audit.WriterController),
		audit.WithActor("system:serviceaccount:krateo-system:core-provider", audit.ClassController))
	r, err := auditrec.NewRecorder(e, metricnoop.NewMeterProvider(), auditrec.LabelsFrom(dyn))
	if err != nil {
		t.Fatal(err)
	}
	const causeUID = "00000000-0000-4000-8000-0000000000cd" // the cause auditRows requires
	cause := audit.CauseRef{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions",
		Namespace: nsName, Name: "fireworksapp", UID: causeUID, Generation: 1}
	ctx := auditrec.WithScope(ctx0, r, cause)

	rules := func(verbs ...string) []rbacv1.PolicyRule {
		return []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: verbs}}
	}
	objs := func(roleVerbs ...string) (rbacv1.ClusterRole, rbacv1.ClusterRoleBinding, rbacv1.Role, rbacv1.RoleBinding, corev1.ServiceAccount) {
		subj := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "fireworksapps-v1-0-0-controller", Namespace: nsName}}
		return rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0"}, Rules: rules("get")},
			rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0"}, Subjects: subj,
				RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "fireworksapps-v1-0-0"}},
			rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0", Namespace: nsName}, Rules: rules(roleVerbs...)},
			rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0", Namespace: nsName}, Subjects: subj,
				RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "fireworksapps-v1-0-0"}},
			corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "fireworksapps-v1-0-0-controller", Namespace: nsName}}
	}
	install := func(roleVerbs ...string) error {
		t.Helper()
		cr, crb, role, rb, sa := objs(roleVerbs...)
		hsh := hasher.NewFNVObjectHash()
		return installRBACResources(ctx, dyn, cr, crb, role, rb, sa, &hsh, kubecli.ApplyOptions{})
	}

	// 1. Create refused: escalation prevention, the writer does not hold configmaps get.
	err = install("get")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("install without configmaps get: err = %v, want the apiserver's Forbidden", err)
	}
	rows := refusalRows(t, sink, causeUID)
	if len(rows) != 1 || rows[0] != (refusal{verb: "create", kind: "ClusterRole", action: auditrec.ActionRBACApply, code: 403}) {
		t.Fatalf("refused create recorded %+v, want exactly one create ClusterRole / failure / 403 with no uid", rows)
	}
	if _, err := admin.Resource(clusterRoleGVR).Get(ctx0, "fireworksapps-v1-0-0", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the refused ClusterRole: Get = %v, want NotFound", err)
	}

	// 2. The writer now holds configmaps get: the same install succeeds, five creates.
	grant(ctx0, t, adminKube, writer+"-configmaps-get", writer, rules("get"))
	waitAllowed(ctx0, t, writerKube, authorizationv1.ResourceAttributes{Group: "", Resource: "configmaps", Verb: "get"}, true)
	sink.Reset()
	if err := install("get"); err != nil {
		t.Fatalf("install holding configmaps get: %v", err)
	}
	if got, want := names(auditRows(t, sink)), []string{"create ClusterRole", "create ClusterRoleBinding", "create Role", "create RoleBinding", "create ServiceAccount"}; !equal(got, want) {
		t.Fatalf("permitted install recorded %v, want %v", got, want)
	}
	liveRole, err := admin.Resource(roleGVR).Namespace(nsName).Get(ctx0, "fireworksapps-v1-0-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Update refused: the Role adds list, which the writer does not hold.
	sink.Reset()
	err = install("get", "list")
	if !apierrors.IsForbidden(err) {
		t.Fatalf("install adding configmaps list: err = %v, want the apiserver's Forbidden", err)
	}
	rows = refusalRows(t, sink, causeUID)
	if len(rows) != 1 || rows[0] != (refusal{verb: "update", kind: "Role", action: auditrec.ActionRBACApply, code: 403, uid: string(liveRole.GetUID())}) {
		t.Fatalf("refused update recorded %+v, want exactly one update Role / failure / 403 with the live uid %s", rows, liveRole.GetUID())
	}
	after, err := admin.Resource(roleGVR).Namespace(nsName).Get(ctx0, "fireworksapps-v1-0-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.GetResourceVersion() != liveRole.GetResourceVersion() {
		t.Fatalf("the refused Role moved: resourceVersion %s -> %s", liveRole.GetResourceVersion(), after.GetResourceVersion())
	}

	// 4. Delete refused: the writer has no delete on clusterroles.
	liveCR, err := admin.Resource(clusterRoleGVR).Get(ctx0, "fireworksapps-v1-0-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sink.Reset()
	cr, crb, role, rb, sa := objs("get")
	err = uninstallRBACResources(ctx, dyn, cr, crb, role, rb, sa, false, false)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("uninstall without clusterroles delete: err = %v, want the apiserver's Forbidden", err)
	}
	rows = refusalRows(t, sink, causeUID)
	if len(rows) != 1 || rows[0] != (refusal{verb: "delete", kind: "ClusterRole", action: auditrec.ActionRBACDelete, code: 403, uid: string(liveCR.GetUID())}) {
		t.Fatalf("refused delete recorded %+v, want exactly one delete ClusterRole / failure / 403 with the live uid %s", rows, liveCR.GetUID())
	}
	if still, err := admin.Resource(clusterRoleGVR).Get(ctx0, "fireworksapps-v1-0-0", metav1.GetOptions{}); err != nil || still.GetDeletionTimestamp() != nil {
		t.Fatalf("the ClusterRole whose delete was refused: err = %v, want it live and not terminating", err)
	}
}

// refusal is one failure record: what was refused, of which object, and the apiserver's answer.
type refusal struct {
	verb, kind, action string
	code               int64
	uid                string
}

// refusalRows is every record in sink, each checked conformant, required to be a failure with cause
// causeUID; a success among them fails the test.
func refusalRows(t *testing.T, sink *audittest.Recorder, causeUID string) []refusal {
	t.Helper()
	var out []refusal
	for _, rec := range sink.Records() {
		audittest.AssertConformant(t, audit.WriterController, rec)
		if got := attrOf(rec, "krateo.audit.outcome"); got != audit.OutcomeFailure {
			t.Errorf("record outcome %q, want %q", got, audit.OutcomeFailure)
		}
		if got := attrOf(rec, "krateo.audit.cause_ref.uid"); got != causeUID {
			t.Errorf("record cause_ref.uid %q, want %q", got, causeUID)
		}
		var code int64
		for _, kv := range audittest.Attributes(rec) {
			if string(kv.Key) == audit.KeyHTTPStatusCode && kv.Value.Type() == attribute.INT64 {
				code = kv.Value.AsInt64()
			}
		}
		out = append(out, refusal{verb: attrOf(rec, "krateo.audit.verb"), kind: attrOf(rec, "krateo.audit.object_ref.kind"),
			action: attrOf(rec, "krateo.action"), code: code, uid: attrOf(rec, "krateo.audit.object_ref.uid")})
	}
	return out
}

// grant binds a new ClusterRole holding rules to user, as admin.
func grant(ctx context.Context, t *testing.T, kube kubernetes.Interface, name, user string, rules []rbacv1.PolicyRule) {
	t.Helper()
	if _, err := kube.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: rules}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// waitAllowed waits until the writer's own SelfSubjectAccessReview for attrs answers want: the
// authorizer reads RBAC from an informer, so a fresh grant is not visible at once.
func waitAllowed(ctx context.Context, t *testing.T, kube kubernetes.Interface, attrs authorizationv1.ResourceAttributes, want bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		rev, err := kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}, metav1.CreateOptions{})
		if err == nil && rev.Status.Allowed == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SelfSubjectAccessReview %s %s/%s: allowed never became %v (last err %v)", attrs.Verb, attrs.Group, attrs.Resource, want, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
