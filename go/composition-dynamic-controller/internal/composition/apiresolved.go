package composition

import (
	"errors"
	"fmt"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/authn"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/snowplow"
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
const ConditionTypeApiResolved = conditionreason.TypeApiResolved

// The literals are declared in internal/conditionreason (core-provider#528).
const (
	reasonApiResolved      = conditionreason.ReasonApiResolved
	reasonApiEmpty         = conditionreason.ReasonApiEmpty
	reasonAuthnUnreachable = conditionreason.ReasonAuthnUnreachable
	reasonAuthnError       = conditionreason.ReasonAuthnError
	reasonAuthnRejected    = conditionreason.ReasonAuthnRejected
	reasonResolveFailed    = conditionreason.ReasonResolveFailed
)

const (
	reasonApiRefUnresolved        event.Reason = "ApiRefUnresolved"
	reasonApiRefPartiallyResolved event.Reason = "ApiRefPartiallyResolved"
)

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
func apiResolvedFailure(err error) metav1.Condition {
	var unreachable *authn.UnreachableError
	if errors.As(err, &unreachable) {
		return conditionreason.ApiAuthnUnreachable.WithMessage(fmt.Sprintf(
			"authn is unreachable, so no service token could be obtained and the apiRef could not "+
				"be resolved. On a remote target this usually means authn is not installed there: %v", err))
	}

	var status *authn.StatusError
	if errors.As(err, &status) {
		switch {
		case status.ServerSide():
			// Keyed on the status CLASS, not on the cause name, so it stays correct against an
			// authn that sends no Status body at all.
			return conditionreason.ApiAuthnError.WithMessage(fmt.Sprintf("authn failed on its own account (HTTP %d%s): %v",
				status.Code, causeSuffix(status), err))
		case status.Cause != "":
			// authn's own discriminator, e.g. NoMapping / AudienceMismatch / MissingBearer.
			return conditionreason.ApiAuthnRefusedWith(status.Cause, fmt.Sprintf("authn refused the service token (HTTP %d): %v",
				status.Code, err))
		case status.Reason != "":
			// A Status body with no causes[]: use authn's top-level reason rather than inventing a
			// specific cause we do not have.
			return conditionreason.ApiAuthnRefusedWith(status.Reason, fmt.Sprintf("authn refused the service token (HTTP %d): %v",
				status.Code, err))
		default:
			// No Status body at all. Reporting AuthnRejected with the code is honest about how
			// little we know; naming a likely cause would be a guess presented as a finding.
			return conditionreason.ApiAuthnRejected.WithMessage(fmt.Sprintf("authn refused the service token (HTTP %d): %v",
				status.Code, err))
		}
	}

	return conditionreason.ApiResolveFailed.WithMessage(fmt.Sprintf("the apiRef could not be resolved: %v", err))
}

// apiResolvedFromOutcomes is the condition for a resolve that answered with data, from snowplow's
// verdict on the RESTAction's stages (Diego, 2026-11-16: True ONLY on `[]`).
//
// A 200 is not a complete resolve. A stage with continueOnError can fail and the reply is still a
// 200, with that stage's data missing: authn pointed a RESTAction at a missing Bundle, snowplow
// logged "partial body served (200)", ApiResolved stayed True and the projected field silently
// went absent. Only the X-Snowplow-Stage-Outcomes header says so, so:
//   - `[]`: True/Resolved;
//   - failed rows or the truncated form: False/ApiPartiallyResolved, naming each stage and its code
//     verbatim (the set is snowplow's and may grow);
//   - no header, or one that cannot be read: Unknown/StageOutcomesUnknown. Never success, per
//     snowplow's own contract, and no version check: a missing header is the capability test.
//
// In every case the data that did resolve is projected. The partial case is the only
// ApiResolved=False with data, which a consumer short-circuiting on False must not miss.
func apiResolvedFromOutcomes(o snowplow.StageOutcomes) metav1.Condition {
	switch {
	case o.Complete():
		return conditionreason.ApiResolved.WithMessage(
			"apiRef resolved and snowplow confirmed every RESTAction stage succeeded; the .api projection source is available")
	case !o.Present:
		return conditionreason.ApiStageOutcomesUnknown.WithMessage(
			"apiRef resolved, but snowplow sent no " + snowplow.StageOutcomesHeader + " header (snowplow older than 1.12.57), " +
				"so whether every RESTAction stage succeeded is unknown; the .api source is projected as received")
	case !o.Known():
		return conditionreason.ApiStageOutcomesUnknown.WithMessage(fmt.Sprintf(
			"apiRef resolved, but snowplow's %s header could not be read (%s), so whether every RESTAction stage succeeded "+
				"is unknown; the .api source is projected as received", snowplow.StageOutcomesHeader, o.Unreadable))
	default:
		n := o.FailedCount()
		stages := "stages"
		if n == 1 {
			stages = "stage"
		}
		return conditionreason.ApiPartiallyResolved.WithMessage(fmt.Sprintf(
			"apiRef resolved partially: %d RESTAction %s did not succeed: %s. The .api source is projected without "+
				"them; status.unrefreshedFields names the status fields this pass could not refresh", n, stages, o.Summary()))
	}
}

func causeSuffix(s *authn.StatusError) string {
	if s.Cause == "" {
		return ""
	}
	return ", " + s.Cause
}

// setApiResolvedCondition upserts the condition. Upsert keys on Type alone, so this coexists with
// Ready and replaces its own previous value across reconciles rather than accumulating.
func setApiResolvedCondition(mg *unstructured.Unstructured, cond metav1.Condition) error {
	return unstructuredtools.SetConditions(mg, cond)
}
