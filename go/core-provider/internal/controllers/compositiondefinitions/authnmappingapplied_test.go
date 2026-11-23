package compositiondefinitions

import (
	"errors"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AuthnMappingApplied, present only with an apiRef (#251 item 2, Diego 2026-11-14), from what was READ of
// the two mappings (#642): the controller's where it runs, core-provider's own on the management cluster.
func TestRecordAuthnMappingApplied(t *testing.T) {
	const (
		present      = deploy.MappingPresent
		missing      = deploy.MappingMissing
		notInstalled = deploy.MappingNotInstalled
		notNeeded    = deploy.MappingNotNeeded
	)
	read := func(s deploy.MappingState) mappingRead { return mappingRead{state: s} }
	unreadable := mappingRead{err: errors.New("get refused")}
	const applied, absent, uninstalled = "True/Applied", "absent", "False/AuthnNotInstalled"
	const spokeMsg, selfMsg = "KubernetesTarget apps/spoke-1 serves no", "core-provider has no authn mapping of its own"

	for name, tc := range map[string]struct {
		controller, self mappingRead
		want             string
		says, saysNot    string
	}{
		"both present":                                      {read(present), read(present), applied, "", ""},
		"no self identity configured":                       {read(present), read(notNeeded), applied, "", ""},
		"the controller's missing":                          {read(missing), read(present), absent, "", ""},
		"core-provider's own missing":                       {read(present), read(missing), absent, "", ""},
		"the controller's unreadable":                       {unreadable, read(present), absent, "", ""},
		"core-provider's own unreadable":                    {read(present), unreadable, absent, "", ""},
		"an unreadable one whose state also says installed": {mappingRead{state: present, err: errors.New("x")}, read(present), absent, "", ""},
		"an error beside notInstalled is not notInstalled":  {read(present), mappingRead{state: notInstalled, err: errors.New("x")}, absent, "", ""},
		"target without authn":                              {read(notInstalled), read(present), uninstalled, spokeMsg, selfMsg},
		"hub without authn":                                 {read(present), read(notInstalled), uninstalled, selfMsg, spokeMsg},
		"neither has authn":                                 {read(notInstalled), read(notInstalled), uninstalled, spokeMsg, ""},
		"known absent wins over unreadable":                 {unreadable, read(notInstalled), uninstalled, selfMsg, ""},
		"known absent wins over missing":                    {read(notInstalled), read(missing), uninstalled, spokeMsg, ""},
	} {
		// From both prior verdicts: a stale one must not survive a read that does not support it.
		for _, prior := range []string{applied, uninstalled} {
			t.Run(name+" after "+prior, func(t *testing.T) {
				cd := apiRefCD(true, "spoke-1")
				if prior == applied {
					cd.SetConditions(cdreason.AuthnMappingApplied.Condition())
				} else {
					cd.SetConditions(cdreason.AuthnNotInstalled.WithMessage("stale"))
				}
				recordAuthnMappingApplied(cd, tc.controller, tc.self)
				got, msg := mappingCondition(cd)
				if got != tc.want || (tc.says != "" && !strings.Contains(msg, tc.says)) || (tc.saysNot != "" && strings.Contains(msg, tc.saysNot)) {
					t.Errorf("%s %q, want %s (says %q, not %q)", got, msg, tc.want, tc.says, tc.saysNot)
				}
			})
		}
	}

	local := apiRefCD(true, "")
	recordAuthnMappingApplied(local, read(notInstalled), read(present))
	if _, m := mappingCondition(local); !strings.HasPrefix(m, "the management cluster serves no") {
		t.Errorf("local: message %q must name the management cluster", m)
	}
	noRef := apiRefCD(false, "spoke-1")
	noRef.SetConditions(cdreason.AuthnMappingApplied.Condition())
	recordAuthnMappingApplied(noRef, read(notNeeded), read(notNeeded))
	if got, _ := mappingCondition(noRef); got != absent {
		t.Errorf("no apiRef: %s, want no condition", got)
	}
}

func apiRefCD(apiRef bool, target string) *compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "fireworksapp", Namespace: "apps"}}
	if apiRef {
		cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "status-sources"}
	}
	if target != "" {
		cd.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: target}}
	}
	return cd
}

func mappingCondition(cd *compositiondefinitionsv1alpha1.CompositionDefinition) (verdict, message string) {
	for _, c := range cd.Status.Conditions {
		if c.Type == conditionTypeAuthnMappingApplied {
			return string(c.Status) + "/" + string(c.Reason), c.Message
		}
	}
	return "absent", ""
}

// Observe writes core-provider's mapping only for a definition the runtime would let Update act on.
func TestObserveMayWriteSelfMapping(t *testing.T) {
	now := metav1.Now()
	for name, tc := range map[string]struct {
		annotations map[string]string
		deleting    bool
		want        bool
	}{
		"plain":                     {nil, false, true},
		"default policy":            {map[string]string{"krateo.io/management-policy": "default"}, false, true},
		"paused":                    {map[string]string{"krateo.io/paused": "true"}, false, false},
		"not paused":                {map[string]string{"krateo.io/paused": "false"}, false, true},
		"observe":                   {map[string]string{"krateo.io/management-policy": "observe"}, false, false},
		"observe,create":            {map[string]string{"krateo.io/management-policy": "observe,create"}, false, false},
		"observe,update":            {map[string]string{"krateo.io/management-policy": "observe,update"}, false, true},
		"does not parse":            {map[string]string{"krateo.io/management-policy": "obsreve"}, false, false},
		"deleting":                  {nil, true, false},
		"orphan on delete is alive": {map[string]string{"krateo.io/deletion-policy": "orphan"}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			cd := apiRefCD(true, "")
			cd.SetAnnotations(tc.annotations)
			if tc.deleting {
				cd.SetDeletionTimestamp(&now)
			}
			if got := observeMayWriteSelfMapping(cd); got != tc.want {
				t.Errorf("may write = %v, want %v", got, tc.want)
			}
		})
	}
}
