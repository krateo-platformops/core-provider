package composition

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// tpObj builds a custom resource at generation 3 with the given status (nil: no status at all).
func tpObj(apiVersion, kind string, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": "x", "namespace": "ns", "generation": int64(3)}}}
	if status != nil {
		u.Object["status"] = status
	}
	return u
}

func cond(typ, status, reason string, og ...int64) map[string]any {
	c := map[string]any{"type": typ, "status": status, "reason": reason}
	if len(og) > 0 {
		c["observedGeneration"] = og[0]
	}
	return c
}

func conds(cs ...map[string]any) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c
	}
	return out
}

// #399. One fixture per shape on the issue: the generic Ready arm, the kagent ModelConfig arm, and
// what must stay unevaluated. The group goes through classifyChild, so the dispatch is tested too.
func TestThirdPartyChildren(t *testing.T) {
	cases := []struct {
		name  string
		group string
		obj   *unstructured.Unstructured
		want  childState
	}{
		// --- generic Ready arm ---
		{"kagent Agent Ready=True (frontend's control)", "kagent.dev",
			tpObj("kagent.dev/v1alpha2", "Agent", map[string]any{"observedGeneration": int64(3),
				"conditions": conds(cond("Accepted", "True", "AgentReconciled"), cond("Ready", "True", "DeploymentReady"))}), childHealthy},
		{"Keycloak Ready=True, top-level observedGeneration", "k8s.keycloak.org",
			tpObj("k8s.keycloak.org/v2alpha1", "Keycloak", map[string]any{"observedGeneration": int64(3),
				"conditions": conds(cond("Ready", "True", ""), cond("HasErrors", "False", ""))}), childHealthy},
		{"CNPG Cluster Ready=True, per-condition observedGeneration only", "postgresql.cnpg.io",
			tpObj("postgresql.cnpg.io/v1", "Cluster", map[string]any{
				"conditions": conds(cond("Ready", "True", "ClusterIsReady", 3), cond("LastBackupSucceeded", "False", "x", 3))}), childHealthy},
		{"CNPG Cluster Ready=False", "postgresql.cnpg.io",
			tpObj("postgresql.cnpg.io/v1", "Cluster", map[string]any{
				"conditions": conds(cond("Ready", "False", "ClusterIsNotReady", 3))}), childConverging},
		{"ExternalSecret Ready=False SecretSyncedError, no observedGeneration", "external-secrets.io",
			tpObj("external-secrets.io/v1", "ExternalSecret", map[string]any{
				"conditions": conds(cond("Ready", "False", "SecretSyncedError"))}), childConverging},
		{"ExternalSecret Ready=True, no observedGeneration anywhere: no gate", "external-secrets.io",
			tpObj("external-secrets.io/v1", "ExternalSecret", map[string]any{
				"conditions": conds(cond("Ready", "True", "SecretSynced"))}), childHealthy},
		{"stale Ready=True (condition og 2, generation 3)", "postgresql.cnpg.io",
			tpObj("postgresql.cnpg.io/v1", "Cluster", map[string]any{
				"conditions": conds(cond("Ready", "True", "ClusterIsReady", 2))}), childConverging},
		{"stale Ready=True (top-level og 2)", "k8s.keycloak.org",
			tpObj("k8s.keycloak.org/v2alpha1", "Keycloak", map[string]any{"observedGeneration": int64(2),
				"conditions": conds(cond("Ready", "True", ""))}), childConverging},
		{"the condition's og wins over a stale top-level og", "example.com",
			tpObj("example.com/v1", "Thing", map[string]any{"observedGeneration": int64(2),
				"conditions": conds(cond("Ready", "True", "", 3))}), childHealthy},

		// --- kagent ModelConfig ---
		{"ModelConfig v1alpha2 Accepted=True/ModelConfigReconciled (057's shape)", "kagent.dev",
			tpObj("kagent.dev/v1alpha2", "ModelConfig", map[string]any{"observedGeneration": int64(3),
				"conditions": conds(cond("Accepted", "True", "ModelConfigReconciled"))}), childHealthy},
		{"ModelConfig v1alpha2 Accepted=False/ModelConfigReconcileFailed", "kagent.dev",
			tpObj("kagent.dev/v1alpha2", "ModelConfig", map[string]any{"observedGeneration": int64(3),
				"conditions": conds(cond("Accepted", "False", "ModelConfigReconcileFailed"))}), childConverging},
		{"ModelConfig v1alpha2 Accepted=True but stale", "kagent.dev",
			tpObj("kagent.dev/v1alpha2", "ModelConfig", map[string]any{"observedGeneration": int64(2),
				"conditions": conds(cond("Accepted", "True", "ModelConfigReconciled"))}), childConverging},
		{"ModelConfig v1alpha3 Accepted=True, ResolvedRefs=False", "kagent.dev",
			tpObj("kagent.dev/v1alpha3", "ModelConfig", map[string]any{
				"conditions": conds(cond("Accepted", "True", "Accepted", 3), cond("ResolvedRefs", "False", "SecretNotFound", 3))}), childConverging},
		{"ModelConfig v1alpha3 Accepted=True, ResolvedRefs=True", "kagent.dev",
			tpObj("kagent.dev/v1alpha3", "ModelConfig", map[string]any{
				"conditions": conds(cond("Accepted", "True", "Accepted", 3), cond("ResolvedRefs", "True", "ResolvedRefs", 3))}), childHealthy},

		// --- stays unevaluated ---
		{"no status (external-secrets Webhook generator)", "generators.external-secrets.io",
			tpObj("generators.external-secrets.io/v1alpha1", "Webhook", nil), childUnevaluated},
		{"Accepted on any OTHER kind is not read generically", "example.com",
			tpObj("example.com/v1", "Route", map[string]any{"conditions": conds(cond("Accepted", "True", "Accepted"))}), childUnevaluated},
		{"conditions without Ready", "example.com",
			tpObj("example.com/v1", "Thing", map[string]any{"conditions": conds(cond("Synced", "True", "ReconcileSuccess"))}), childUnevaluated},
		{"phase only, no conditions (mongodbcommunity, #121)", "mongodbcommunity.mongodb.com",
			tpObj("mongodbcommunity.mongodb.com/v1", "MongoDBCommunity", map[string]any{"phase": "Failed"}), childUnevaluated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyChild(c.group, c.obj); got != c.want {
				t.Errorf("classifyChild(%s %s) = %v, want %v", c.group, c.obj.GetKind(), got, c.want)
			}
		})
	}
}

// The generic arm is ONLY for third parties: a krateo.io child keeps krateoReady's reason mapping,
// so a krateo child's Ready=False/Unavailable is still a FAILURE, not the generic arm's converging.
func TestKrateoChildrenKeepTheirOwnReasons(t *testing.T) {
	obj := tpObj("composition.krateo.io/v1", "Portal", map[string]any{"conditions": conds(cond("Ready", "False", "Unavailable"))})
	if got := classifyChild("composition.krateo.io", obj); got != childFailed {
		t.Errorf("krateo child Ready=False/Unavailable = %v, want childFailed (krateoReady, not the third-party arm)", got)
	}
}
