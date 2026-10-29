package composition

import (
	"errors"
	"fmt"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/authn"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ConditionTypeApiResolved reports whether the declared apiRef could be resolved into the ".api"
// projection source. Present whenever an apiRef is declared, True or False — never absent on
// success, so a consumer can distinguish "resolved" from "nobody looked" (#251).
//
// It is a SEPARATE condition type from Ready, deliberately. The degrade is intentional: a resolver
// failure must not make a composition whose helm release is healthy report unready. So Ready keeps
// describing the release and ApiResolved describes the projection source, and a consumer that
// gates on Ready is unaffected by adding this.
const ConditionTypeApiResolved = "ApiResolved"

const (
	reasonApiResolved = "Resolved"
	reasonApiEmpty    = "ApiEmpty"

	// AuthnUnreachable is the one an operator most needs to see: on a spoke it means no authn was
	// installed into this target at all, which is the failure that used to be silent.
	reasonAuthnUnreachable = "AuthnUnreachable"
	// AuthnError is authn failing on its own account (5xx), NOT refusing the caller. A rejection is
	// about the caller; a 5xx is about authn, and conflating them sends an operator to check
	// audiences and mappings when the fault is authn's own dependency.
	reasonAuthnError = "AuthnError"
	// AuthnRejected is the fallback for a refusal whose body carries no machine-readable cause —
	// an authn older than 0.28.2, which is what runs until the next umbrella roll.
	reasonAuthnRejected = "AuthnRejected"
	// ResolveFailed is everything that is not an authn problem: snowplow unreachable, a RESTAction
	// that does not resolve, a malformed apiRef.
	reasonResolveFailed = "ResolveFailed"
)

const reasonApiRefUnresolved event.Reason = "ApiRefUnresolved"

// apiResolvedFailure classifies a resolution error into a condition reason and message.
//
// Classification is by TYPE via errors.As, not by matching message text: the snowplow client wraps
// the authn error ("acquiring snowplow token: %w") before it ever reaches here, and a message
// reworded upstream must not silently reclassify a failure.
//
// authn's cause names are passed through VERBATIM rather than remapped into a local vocabulary.
// authn commits to adding names and never renaming them, so pass-through means a new cause of
// theirs appears in this condition the day they ship it, with no change here. Remapping would make
// their docs and these conditions diverge on the first addition.
func apiResolvedFailure(err error) (reason, message string) {
	var unreachable *authn.UnreachableError
	if errors.As(err, &unreachable) {
		return reasonAuthnUnreachable, fmt.Sprintf(
			"authn is unreachable, so no service token could be obtained and the apiRef could not "+
				"be resolved. On a remote target this usually means authn is not installed there: %v", err)
	}

	var status *authn.StatusError
	if errors.As(err, &status) {
		switch {
		case status.ServerSide():
			// Keyed on the status CLASS, not on the cause name, so it stays correct against an
			// authn that sends no Status body at all.
			return reasonAuthnError, fmt.Sprintf("authn failed on its own account (HTTP %d%s): %v",
				status.Code, causeSuffix(status), err)
		case status.Cause != "":
			// authn's own discriminator, e.g. NoMapping / AudienceMismatch / MissingBearer.
			return status.Cause, fmt.Sprintf("authn refused the service token (HTTP %d): %v",
				status.Code, err)
		case status.Reason != "":
			// A Status body with no causes[]: use authn's top-level reason rather than inventing a
			// specific cause we do not have.
			return status.Reason, fmt.Sprintf("authn refused the service token (HTTP %d): %v",
				status.Code, err)
		default:
			// No Status body at all. Reporting AuthnRejected with the code is honest about how
			// little we know; naming a likely cause would be a guess presented as a finding.
			return reasonAuthnRejected, fmt.Sprintf("authn refused the service token (HTTP %d): %v",
				status.Code, err)
		}
	}

	return reasonResolveFailed, fmt.Sprintf("the apiRef could not be resolved: %v", err)
}

func causeSuffix(s *authn.StatusError) string {
	if s.Cause == "" {
		return ""
	}
	return ", " + s.Cause
}

// setApiResolvedCondition upserts the condition. Upsert keys on Type alone, so this coexists with
// Ready and replaces its own previous value across reconciles rather than accumulating.
func setApiResolvedCondition(mg *unstructured.Unstructured, status metav1.ConditionStatus, reason, message string) error {
	return unstructuredtools.SetConditions(mg, metav1.Condition{
		Type:               ConditionTypeApiResolved,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}
