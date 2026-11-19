package compositiondefinitions

import (
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #251 item 2, Diego 2026-11-14: AuthnMappingApplied, present only with an apiRef. True/Applied when
// the mapping CRD exists where the controller runs; False/AuthnNotInstalled naming that cluster when it
// does not; removed when the definition has no apiRef.
func TestRecordAuthnMappingApplied(t *testing.T) {
	cdWith := func(apiRef bool, target string) *compositiondefinitionsv1alpha1.CompositionDefinition {
		cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "fireworksapp", Namespace: "apps"}}
		if apiRef {
			cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "status-sources"}
		}
		if target != "" {
			cd.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: target}}
		}
		return cd
	}
	get := func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) (string, string, string, bool) {
		for _, c := range cd.Status.Conditions {
			if c.Type == conditionTypeAuthnMappingApplied {
				return string(c.Status), string(c.Reason), c.Message, true
			}
		}
		return "", "", "", false
	}

	cd := cdWith(true, "spoke-1")
	recordAuthnMappingApplied(cd, deploy.AuthnSupport{NotInstalled: true})
	if st, r, m, ok := get(cd); !ok || st != "False" || r != string(rtv1.ConditionReason(cdreason.AuthnNotInstalled.String())) || !strings.Contains(m, "KubernetesTarget apps/spoke-1") {
		t.Errorf("remote target without authn: %s/%s %q (present %v), want False/AuthnNotInstalled naming KubernetesTarget apps/spoke-1", st, r, m, ok)
	}

	cd = cdWith(true, "")
	recordAuthnMappingApplied(cd, deploy.AuthnSupport{NotInstalled: true})
	if _, _, m, _ := get(cd); !strings.Contains(m, "the management cluster") {
		t.Errorf("local without authn: message %q must name the management cluster", m)
	}

	recordAuthnMappingApplied(cd, deploy.AuthnSupport{})
	if st, r, _, ok := get(cd); !ok || st != "True" || r != string(rtv1.ConditionReason(cdreason.AuthnMappingApplied.String())) {
		t.Errorf("authn installed: %s/%s (present %v), want True/Applied", st, r, ok)
	}

	noRef := cdWith(false, "spoke-1")
	recordAuthnMappingApplied(noRef, deploy.AuthnSupport{NotInstalled: true})
	if _, _, _, ok := get(noRef); ok {
		t.Error("a definition without an apiRef has no mapping, so it must not carry the condition")
	}
}
