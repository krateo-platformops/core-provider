package composition

import (
	"context"
	"fmt"
	"strings"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// childState is a managed child's rolled-up health.
type childState int

const (
	childHealthy childState = iota
	childConverging
	childFailed
	// childUnevaluated: the child exists, but nothing here can say whether it is healthy — a custom
	// resource whose status vocabulary only its blueprint knows, or one we could not read.
	//
	// It rolls up exactly like childHealthy, so it NEVER affects Ready. Its whole purpose is to be
	// counted: tier 2 of krateo-core-provider#121 depends on blueprint authors attaching a RESTAction
	// to their CompositionDefinition, and adoption is 0 of 44 because omitting it fails SILENTLY —
	// the composition just reads green, so the author never sees the gap. Counting these makes the
	// absence visible without inventing a verdict we are not entitled to.
	childUnevaluated

	// childAbsent: recorded in status.managed and NOT FOUND in the cluster. It rolls up exactly
	// like childConverging — Ready=False/Creating, named not counted — because "just applied,
	// self-heals" is the common reason for it and that must not become terminal.
	//
	// It is tracked SEPARATELY because it is also the signature of a stale status.managed, and
	// those two are indistinguishable from the child alone (#261). A chart that RENAMES a child
	// leaves the old name recorded and the new one absent, and if the digest is unchanged no
	// Update is ever dispatched, so the composition names a deleted resource forever. Observe uses
	// this to report drift instead, which hands the repair to Update — the only phase that
	// rewrites status.managed.
	//
	// Deliberately not inferred from `converging`: a child that EXISTS and is merely unready must
	// not dispatch an Update on every resync, which is why this is a state and not a predicate
	// over the existing list.
	childAbsent

	// childUnreadable: the controller's ServiceAccount was REFUSED a read of this child
	// (apierrors.IsForbidden). Scoped to Forbidden ONLY — a missing REST mapping or a transient GET
	// failure keeps the old silent childHealthy treatment, because neither is actionable and
	// neither is ours.
	//
	// SEPARATE FROM childUnevaluated, and the distinction is the whole of #318. A widget publishing
	// `status: {}` is unevaluated: honest, and the child's own property. A child behind a missing
	// grant is unreadable, which is a DEFECT IN GENERATED RBAC — ours, and actionable. Folding
	// them together would lose exactly the distinction the issue exists to create.
	//
	// Before this state existed, a Forbidden read returned childHealthy and was counted NOWHERE:
	// only childUnevaluated incremented a counter, so one unreadable child and 121 unreadable
	// children produced byte-identical output, `Ready=True / Available` with the same message. The
	// mirror image of the stale green this file spent #248/#261/#269/#273 removing — a parent
	// reading healthy for a child nobody could read. Named by the provider-runtime session.
	//
	// Nobody has ever measured this population, because nothing counted it. It may be zero. The
	// unevaluated numbers in rollupManagedChildren's comment (396 of 725 children on krateo-057,
	// 631 of 790 on day2, 118 of 121 on one composition) are the reason not to assume so.
	childUnreadable
)

// nativeGroups are the API groups whose readiness semantics Kubernetes itself defines, so this
// controller can legitimately interpret them (tier 1). Everything else is a custom resource whose
// vocabulary belongs to whoever wrote the CRD.
//
// "" is core/v1 and the *.k8s.io groups are most of the rest, but NOT all of them: `apps`, `batch`,
// `autoscaling`, `policy` and the legacy `extensions` are built-in and carry no suffix. Testing the
// suffix alone therefore called them custom, and that was a latent false negative — it did not show
// until #256 made unmodelled apps and batch kinds fall through to here, at which point they were
// classified childUnevaluated instead of childHealthy.
//
// The distinction matters even though both roll up without affecting Ready: childUnevaluated is
// COUNTED as a gap, and that count is the adoption signal for blueprint authors attaching a
// RESTAction (#121). Counting a built-in Kubernetes kind as a gap tells an author to describe
// readiness for a resource that has none, so the number stops meaning what it is read as meaning.
//
// A child in one of these groups that we do not model (ConfigMap, Service, RBAC, CRD,
// ControllerRevision...) is not a gap — those kinds carry no readiness at all, so existence
// genuinely is health for them.
func isNativeGroup(group string) bool {
	switch group {
	case "", "apps", "batch", "autoscaling", "policy", "extensions":
		return true
	}
	return strings.HasSuffix(group, ".k8s.io")
}

// healthVerdict is the composition-level rollup of its managed children.
type healthVerdict struct {
	ready   bool
	reason  string // "Available" | "Creating" | "Unavailable"
	message string
	failing []string
	// converging names children that are not ready YET, as failing names those that have failed.
	// Both are named rather than counted: a count tells an operator that something is wrong and
	// gives them no way to reach it (#144 discussion on 057).
	converging []string
	// unevaluated counts children whose health could not be assessed. Reported, never acted on.
	unevaluated int
	// unreadable names children the ServiceAccount was REFUSED a read of (#318). NAMED, not just
	// counted, for the #289 reason the converging list already records: a count could never have
	// been produced without knowing which child it was, and not saying so was the whole defect.
	// The count reaches consumers as status.unreadableChildrenCount, because prose is not an
	// interface; the names reach an operator through the message, because names do not belong in
	// a scalar.
	unreadable []string
	// absent names children recorded in status.managed that do not exist. A SUBSET of converging —
	// readiness treats them identically — surfaced separately so Observe can report drift and let
	// Update rewrite the recorded set (#261). Not part of the condition message.
	absent []string
	// total is len(status.managed), kept so the unevaluated caveat can be composed by resolveReady
	// rather than baked into the rollup's own message — which resolveReady discards.
	total int
	// previousTotal is status.previousManagedCount: how many children the inventory listed BEFORE
	// the last render replaced it. Zero when never recorded.
	//
	// Kept here for the same reason as total — so the shrink caveat is composed by resolveReady,
	// which is the only place a message survives (#230). Read from status rather than compared
	// in-flight because Observe stamps the message on every reconcile and has no `previous`.
	previousTotal int
}

// rollupManagedChildren GETs each child listed in status.managed and rolls their health into one
// verdict. It is deliberately FAIL-SAFE: a child it cannot read (Forbidden — the controller SA lacks
// the grant) or whose kind it does not model is counted HEALTHY, so the rollup can never flip a
// working composition to Unavailable; it can only surface children it positively observes are sick
// (see krateo-core-provider#72, #73).
//
// NotFound is the exception and is NOT healthy: it means "just applied, self-heals", so it counts as
// CONVERGING and the composition reports Ready=False with reason Creating. An earlier version of this
// comment grouped NotFound with Forbidden as healthy, which was wrong.
//
// WHAT THE NAMED SETS MEAN, because this is the sentence a future editor can violate without
// noticing: `failing` and `converging` name children we could READ that were unready or absent.
// Every non-NotFound GET error degrades to healthy and is SILENT — nothing names it. So absence from
// those lists is NOT evidence of health. The list is a floor, not a census.
//
// That silence is deliberate and must stay. The obvious "improvement" is to name unreadable children
// too, on the grounds that an unreadable child is a problem worth reporting. Do not: a controller SA
// missing one RBAC grant would then turn every composition using that kind Ready=False at once,
// converting an RBAC gap into a fleet-wide outage. A missing grant must not regress a working
// composition — that is the whole point of the fail-safe, and naming is the one change that can undo
// it while looking like a fix.
func (h *handler) rollupManagedChildren(ctx context.Context, dyn dynamic.Interface, mg *unstructured.Unstructured) healthVerdict {
	managed, found, _ := unstructured.NestedSlice(mg.Object, "status", "managed")
	if found && len(managed) == 0 {
		// PRESENT AND EMPTY is a real answer: a reconcile decoded the release, rendered nothing,
		// and recorded that. There are no children, so there is nothing to be unhealthy, and
		// "up-to-date" is honest. This is the case an earlier author reasoned about and guarded,
		// and it stays Available.
		//
		// It is only distinguishable from the absent case because setStatus now records an empty
		// render explicitly (#248). Before that, `managed: []` was never written and this branch
		// was unreachable.
		return healthVerdict{ready: true, reason: "Available", message: readyMessageAvailable}
	}
	if !found {
		// NO RECORDED CHILDREN IS NOT EVIDENCE OF HEALTH (#248). This branch used to return
		// Available with "Composition is up-to-date", which answered "I have no information" with
		// "everything is fine" — the same shape as the stampReady default arm closed in #244, one
		// layer lower and with an absent INPUT rather than an unrecognised output.
		//
		// It was not hypothetical. On krateo-057, 7 of 83 compositions had no `status.managed` key
		// at all while reporting Ready=True/Available over 506 objects their own deployed Helm
		// manifests show they render — observability-page 145, portal-agents 121,
		// agentgateway-policies 94. Observe passes `resources: nil` on the assumption that
		// Create/Update already recorded them (composition.go), so nothing ever repairs the field:
		// the green was self-perpetuating, and three consumers believed it (deps: ordering, the
		// installer's readiness gate, and the portal's `ready` bucket).
		//
		// ZeroEvidence is Unknown, not False — deliberately. The state is steady, not transient, so
		// False would wedge every consumer gating on readiness, which is the one direction that
		// turns an honesty fix into an outage (the wedge this file documents for #72, #112, #165).
		//
		// ABSENT is not the same as EMPTY, and the branch above is why this one can be strict.
		// `setStatus` now records an empty render as `managed: []` (keyed on recordManaged), so a
		// chart that genuinely renders nothing lands in the Available branch above. Reaching HERE
		// means no reconcile has ever recorded the list -- which is a different claim from "there
		// is nothing to record", and the only one of the two that is a defect.
		return healthVerdict{
			ready:  false,
			reason: ReasonInventoryNotRecorded,
			message: "Composition health is unknown: no managed children are recorded, so none " +
				"could be health-evaluated. The inventory has never been written, which is a " +
				"defect in this controller rather than a property of the composition",
		}
	}

	var failing, converging []string
	unevaluated := 0
	var absent, unreadable []string
	for _, m := range managed {
		ref, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch h.childHealth(ctx, dyn, ref) {
		case childFailed:
			failing = append(failing, childID(ref))
		case childConverging:
			// Named, not just counted. The previous "%d of %d managed children are not ready"
			// gave an operator a number and no way to reach the child: status.managed lists
			// Kubernetes objects with no readiness field to sort by, so finding the one meant
			// sweeping every kind in composition.krateo.io and checking each CR's own conditions —
			// ~40 kinds, ambiguous because compositions flap Ready=False transiently during a roll.
			// That produced three wrong attributions in one evening on 057, including blaming a
			// composition that was not in status.managed at all.
			//
			// The count could never have been produced without knowing which child it was. Not
			// saying so was the whole defect, and the asymmetry made it obvious in hindsight: the
			// childFailed branch below has named its children all along.
			converging = append(converging, childID(ref))
		case childAbsent:
			// Identical readiness treatment to childConverging: named in the same list so the
			// message and the verdict are unchanged. The only difference is that `absent` is also
			// recorded, for Observe's drift decision (#261).
			converging = append(converging, childID(ref))
			absent = append(absent, childID(ref))
		case childUnevaluated:
			unevaluated++ // counted only; never changes the verdict
		case childUnreadable:
			// Counted AND named, and like childUnevaluated it does not enter `failing` or
			// `converging`, so it cannot by itself fail or hold back a parent. The only verdict it
			// participates in is the nothing-was-assessed case below.
			unreadable = append(unreadable, childID(ref))
		}
	}

	switch {
	case len(failing) > 0:
		return healthVerdict{
			absent:     absent,
			ready:      false,
			reason:     "Unavailable",
			message:    fmt.Sprintf("managed children not healthy: %s", joinCap(failing, 3)),
			failing:    failing,
			unreadable: unreadable,
			total:      len(managed),
		}
	case len(converging) > 0:
		return healthVerdict{
			ready:  false,
			reason: "Creating",
			message: fmt.Sprintf("%d of %d managed children are not ready: %s",
				len(converging), len(managed), joinCap(converging, 3)),
			converging: converging,
			absent:     absent,
			unreadable: unreadable,
			total:      len(managed),
		}
	case unevaluated+len(unreadable) == len(managed):
		// ZERO EVIDENCE (#165). Nothing was evaluated, so "Available" is a claim about an empty
		// set. The caveat already said so in the message and the condition said Available anyway,
		// which is the combination nobody reads as a warning.
		//
		// Unknown, NOT False. Zero evidence is a steady state, not a transient one — a blueprint
		// whose children are all custom resources never becomes interpretable on its own — so
		// False would block every consumer gating on readiness, indefinitely, on a composition
		// that is as far as anyone knows fine. That is the wedge this file already documents four
		// times over (#72's snowplow, #112's CronJob), and the one direction that turns an
		// honesty fix into an outage.
		//
		// Deliberately ONLY the all-or-nothing case. 32 of 33 compositions carrying the caveat on
		// krateo-057 are partial, the largest at 142 of 145, and those keep reporting Available:
		// evidence from three children is thin but it is not nothing, and there is no principled
		// threshold between 142/145 and 5/5 that is not just a number someone picked.
		//
		// #318 WIDENED THE ARM AND SPLIT THE REASON. It used to read `unevaluated == len(managed)`,
		// because an unreadable child was counted healthy and so could never reach here. Now that
		// Forbidden reads have their own state, "nothing was assessed" has two causes and they are
		// not equally actionable:
		//
		//	ZeroEvidence       the children published nothing this controller can read.
		//	                   The children's own property. Not ours to fix.
		//	ChildrenForbidden  the ServiceAccount was refused a read. A defect in the RBAC we
		//	                   GENERATE, and the one an operator can act on.
		//
		// Diego's rule, in the form that does not leave a hole: Unknown when NOT ONE child was
		// assessed. Keying it on `len(unreadable) == len(managed)` alone would have left a
		// composition with 3 unreadable and 2 unevaluated children reporting Available with zero
		// evidence behind it, which is the defect wearing a different mix.
		//
		// WHY THE REASON MATTERS MORE THAN USUAL HERE: installer's gate accepts Ready=Unknown ONLY
		// with reason exactly ZeroEvidence, with no else-arm (chart/templates/_helpers.tpl,
		// inst.readyFromConditions, verified independently by two sessions against the live repo).
		// So this reason string decides whether dependents are held back, and installer has
		// allowlisted ChildrenForbidden ahead of this landing (their #646, 8bbeb81) — the safe
		// ordering, since a reason nothing emits is never matched. A near-miss spelling BLOCKS
		// rather than silently claiming readiness, which is why their test asserts
		// `Unknown/Forbidden` must block.
		reason := condition.ReasonZeroEvidence
		message := zeroEvidenceMessage(len(managed))
		if len(unreadable) > 0 {
			reason = ReasonChildrenForbidden
			message = childrenForbiddenMessage(unreadable, unevaluated, len(managed))
		}
		return healthVerdict{
			ready: false, reason: reason,
			message:     message,
			unevaluated: unevaluated,
			unreadable:  unreadable,
			total:       len(managed),
		}
	default:
		// Green — but say so honestly. If some children could not be assessed, "up-to-date" is a
		// claim about the ones we could read, and silently omitting that is what let 0 of 44
		// blueprints notice they never attached a RESTAction (#121 tier 2).
		return healthVerdict{
			ready: true, reason: "Available",
			message:     availableMessage(unevaluated, len(managed)),
			unevaluated: unevaluated,
			// CARRIED EVEN THOUGH THE VERDICT IS GREEN, which is the whole point of #318 short of
			// the all-or-nothing case: a composition with 6 of 10 children unreadable still reports
			// Available — Diego's decision, and the narrowest one — but it now says so, in
			// status.unreadableChildrenCount and by name in the message. One unreadable child and
			// 121 no longer produce identical output.
			unreadable: unreadable,
			total:      len(managed),
		}
	}
}

// zeroEvidenceMessage says what is and is not known, without implying the composition is broken.
// It must not read as a failure: an operator seeing it has a coverage gap, not an outage.
func zeroEvidenceMessage(total int) string {
	return fmt.Sprintf("Composition health is unknown: none of its %d managed children could be "+
		"health-evaluated (no readiness semantics this controller knows how to read)", total)
}

// childrenForbiddenMessage says that NOTHING was assessed. It deliberately does not name the
// refused children or say that RBAC is why: withUnreadable appends both, on every arm, so there is
// one format and one place that can be wrong. Saying it here too would print it twice on this arm
// and nowhere on the others, which is the state this replaced.
func childrenForbiddenMessage(unreadable []string, unevaluated, total int) string {
	msg := fmt.Sprintf("Composition health is unknown: none of its %d managed children could be "+
		"health-evaluated", total)
	if unevaluated > 0 {
		msg += fmt.Sprintf(" (%d of them publish no readiness semantics this controller knows how "+
			"to read)", unevaluated)
	}
	return msg
}

// availableMessage qualifies the healthy message when some children could not be assessed, so the
// gap is visible at the point people actually look (the Ready condition, which the portal renders).
func availableMessage(unevaluated, total int) string {
	return withUnevaluated("Composition is up-to-date", unevaluated, total)
}

// withUnevaluated appends the coverage caveat to whatever message is being stamped.
//
// It is a caveat about OUR OWN coverage, not a competing claim about health — which is why it
// appends rather than replaces. Getting that backwards is how it went missing: the rollup baked the
// caveat into its own message, resolveReady then chose between the projected and the phase message
// and never read the rollup's, so the caveat was computed on every reconcile and discarded every
// time (#121 tier 2 shipped unreachable; found while answering how childFailed coexists with the
// projection).
func withUnevaluated(base string, unevaluated, total int) string {
	if unevaluated == 0 {
		return base
	}
	return fmt.Sprintf("%s (%d of %d managed children not health-evaluated)", base, unevaluated, total)
}

// withShrunkSet appends the denominator to the message when the inventory lists FEWER children than
// it did before the last render (#230).
//
// `Ready=True` asserts "what I render is healthy". A reader takes it to mean "the composition is
// complete", and those differ exactly during a multi-component version bump — which is the moment
// anyone is watching. 122 -> 108 -> 122 is indistinguishable from 122 -> 122 in the condition, and a
// post-roll gate read five minutes in reported 14 of 17 components as DISAPPEARED when they were
// simply in the prune window. The reporter nearly escalated a 14-component outage that did not exist.
//
// Like withUnevaluated this APPENDS and changes no verdict. It is a statement about the set the
// rollup examined, not a competing claim about health, and a human who reads it waits instead of
// escalating. It deliberately does NOT protect an automated gate: `inst.depsReady` keys on the
// condition and never reads messages. That remains open as a verdict change, which is a decision on
// a hot path and not something to slip into a message fix.
func withShrunkSet(base string, total, previousTotal int) string {
	if previousTotal <= total {
		return base
	}
	return fmt.Sprintf("%s (%d of %d previously-managed children rendered)", base, total, previousTotal)
}

// readyOutcome is the final Ready decision: a reason ("Available" | "Creating" | "Unavailable" |
// "ZeroEvidence") and the message to stamp.
type readyOutcome struct {
	reason  string
	message string
}

// projectedHealth is a blueprint's OWN declared health, read from .status.health. Only a health
// RESTAction projection writes there, so `present` unambiguously means "the author has a view".
//
// Grouped into a struct rather than threaded as positional arguments because `reason` arriving
// (#234) would have made resolveReady a five-scalar signature with two bools and three strings
// adjacent — the shape where a swapped argument compiles and lies.
type projectedHealth struct {
	present bool
	ready   bool
	// reason is OPTIONAL. Absent — which is every blueprint written before #234 — it changes
	// nothing at all; see notReadyReason.
	reason  string
	message string
}

// notReadyReason maps a declared not-ready projection onto the Ready reason to stamp.
//
// WHY THIS EXISTS (krateo-core-provider#234). `.status.health.ready: false` had exactly one
// meaning: Unavailable, i.e. broken. A composition that is mid-bump — its own values changed and
// its children not yet reconverged — had no way to say so, and the installer's #230 fix needs to.
// Reporting Unavailable there propagates a FAULT: krateoReady maps a child whose Ready reason is
// Unavailable to childFailed, so one deliberately-converging composition turned every parent into
// an outage report. A false green is a missed signal; a false red is a page.
//
// Only "Creating" is recognised, and an absent or unrecognised reason yields Unavailable —
// today's behaviour, unchanged. That asymmetry is deliberate and is the safe direction: a typo
// read as Creating would quietly downgrade a real failure to "still converging" and wait forever,
// whereas a typo read as Unavailable is the loud answer the author already got before #234.
//
// The accepted spelling must equal what condition.Creating() stamps as its Reason, because the
// round trip is the whole feature: this reason is written onto the child's Ready condition, and a
// PARENT then reads it back through krateoReady. TestDeclaredConverging_RoundTripsToChildConverging
// pins both ends of that trip; a rename on either side fails there.
func (p projectedHealth) notReadyReason() string {
	if strings.EqualFold(p.reason, "Creating") {
		return "Creating"
	}
	return "Unavailable"
}

// resolveReady folds a blueprint's projected health (.status.health, when present) with the
// generic managed-child rollup into the final Ready outcome (krateo-core-provider#96):
//
//   - A blueprint that projected ready=false is honored — the author declared it not ready, and
//     their optional reason decides whether that reads as broken or as still converging (#234).
//   - Otherwise a positively-observed sick managed child (a workload with unready/unavailable
//     replicas, a failed Job, a nested composition reporting Unavailable) keeps the composition
//     Ready=False EVEN WHEN the blueprint projected ready=true. Projected health may VETO but must
//     not override observed workload sickness to Ready: "applied" is not "serving", and a `deps:`
//     edge must wait for the workload to actually be healthy.
//   - When everything observed is healthy, the composition is Available, preferring the author's
//     projected message when they supplied one.
//
// The rollup is itself fail-safe (unreadable / unmodeled children count healthy), so this can only
// surface failures it can positively observe — it never flips a working composition to NotReady.
// resolveReady is the verdict below plus ONE exit that appends the unreadable-children caveat.
//
// WHY A WRAPPER AND NOT A LINE IN EACH ARM. The provider-runtime session's condition on #318's
// narrow threshold: below the all-or-nothing case the verdict must not move, but the unreadable
// children must still be NAMED, or "one of 121 unreadable" goes back to silence, which is the
// defect #318 exists to remove. They were right, and my first implementation did not do it — the
// names reached the message only on the ChildrenForbidden arm, and the Available, Unavailable and
// Creating arms dropped them while a comment four lines away claimed they were "named in the
// message". A comment claiming more than its code.
//
// resolveReady has seven returns. Appending in each is how an arm gets missed, and the arm that
// gets missed is the silent one — there is no symptom to notice. One exit cannot be forgotten.
func resolveReady(p projectedHealth, defaultMsg string, v healthVerdict) readyOutcome {
	out := resolveReadyVerdict(p, defaultMsg, v)
	out.message = withUnreadable(out.message, v.unreadable, v.total)
	return out
}

// withUnreadable appends the refused children to whatever message is being stamped, like
// withUnevaluated and withShrunkSet and for the same reason: it is a caveat about OUR OWN access,
// not a competing claim about health, so it appends and changes no verdict.
//
// THE ONLY PLACE THAT NAMES THEM, deliberately. childrenForbiddenMessage states the CAUSE and
// leaves the naming here, so there is exactly one format and no arm can both name them and not.
//
// Capped at 3 like every other list: a composition with 118 refused children must not put 118
// names into a condition etcd rewrites on every reconcile. The count that a CONSUMER reads is
// status.unreadableChildrenCount — this string is mine to reword and prose is not an interface.
func withUnreadable(base string, unreadable []string, total int) string {
	if len(unreadable) == 0 {
		return base
	}
	return fmt.Sprintf("%s (%d of %d managed children could NOT BE READ — this controller's "+
		"ServiceAccount was refused access, which is a gap in the generated RBAC rather than a "+
		"problem with the children: %s)", base, len(unreadable), total, joinCap(unreadable, 3))
}

func resolveReadyVerdict(p projectedHealth, defaultMsg string, v healthVerdict) readyOutcome {
	if p.present && !p.ready {
		msg := p.message
		if msg == "" {
			msg = defaultMsg
		}
		// A positively-observed TERMINAL failure outranks whatever the author declared. This is the
		// same asymmetry the ready=true case already encodes — a projection may veto health but may
		// not overrule what we can see — applied to #234's new direction: "I am converging" must not
		// mask a child we positively observed has failed and will not recover on its own.
		//
		// Both signals are kept. The author's verdict is the primary reason and leads; the rollup's
		// names are the part an operator can act on, and returning msg alone discarded them — the
		// one case where the two agree was the one case that lost the identifiers.
		if len(v.failing) > 0 {
			return readyOutcome{
				reason:  "Unavailable",
				message: fmt.Sprintf("%s; also unhealthy: %s", msg, joinCap(v.failing, 3)),
			}
		}
		return readyOutcome{reason: p.notReadyReason(), message: msg}
	}
	switch v.reason {
	case "Unavailable":
		return readyOutcome{reason: "Unavailable", message: v.message}
	case "Creating":
		return readyOutcome{reason: "Creating", message: v.message}
	case condition.ReasonZeroEvidence, ReasonInventoryNotRecorded, ReasonChildrenForbidden:
		// ChildrenForbidden JOINED THIS ARM LATE (#430). The rollup has returned it since #318, but
		// this switch had no case for it, so it fell to the Available tail below: a composition whose
		// every child read was refused reported Ready=True/Available, the opposite of the Unknown that
		// #318 decided and installer allowlisted. The tests checked the rollup's reason, never what
		// reached the condition; TestEveryReasonTheRollupReturnsIsHandledByResolveReady now walks the
		// rollup's reasons, the domain the stampReady guard never covered.
		// A blueprint that projected ready=TRUE is honoured here and the composition is Available:
		// the author's health RESTAction IS evidence, and it is better evidence than the generic
		// rollup's, so a projection turns zero evidence into one positive signal. This branch is
		// only reached when the projection is absent or says ready, and in the latter case the
		// author has told us it is healthy.
		if p.present && p.ready {
			msg := p.message
			if msg == "" {
				msg = defaultMsg
			}
			return readyOutcome{reason: "Available", message: msg}
		}
		// TWO LITERALS, NOT v.reason, and the exhaustiveness test is why. It walks this function's
		// AST to enumerate every reason resolveReady can produce and asserts stampReady handles
		// each one; a variable defeats the walk, and it REFUSED rather than claiming the two sides
		// agree ("the walk could not resolve 1 reason expression"). A guard that cannot read the
		// code it guards must not pass, and this one does not.
		if v.reason == ReasonInventoryNotRecorded {
			return readyOutcome{reason: ReasonInventoryNotRecorded, message: v.message}
		}
		if v.reason == ReasonChildrenForbidden {
			return readyOutcome{reason: ReasonChildrenForbidden, message: v.message}
		}
		return readyOutcome{reason: condition.ReasonZeroEvidence, message: v.message}
	}
	// Available. The base is the caller's message, or the author's projected message when they
	// wrote one.
	//
	// THE PHASE NO LONGER VARIES THIS, AND THAT IS THE POINT (#298). Observe, Create and Update used
	// to supply three different strings for one Available state — "Composition is up-to-date",
	// "Composition created", "Composition values updated". setReadyCondition's guard is
	// `cur.Message == message && cur.Status == cond.Status`, so whenever the phases alternated the
	// message differed every time, the guard could never fire, and the condition was rewritten with a
	// fresh LastTransitionTime on every reconcile. The timestamp then claimed the composition had
	// just become ready when it had been ready for hours — provably so: a sibling's pods were Ready
	// ten minutes before one such condition's own timestamp.
	//
	// A condition's message describes the STATE; the phase that happened to observe it is not part
	// of the state. Which phase ran is already recorded in events and logs, and freshness now has
	// its own field (status.lastReconcileTime), so nothing is lost by making this stable.
	msg := defaultMsg
	if p.present && p.message != "" {
		msg = p.message
	}
	// Both caveats, innermost first: the shrink is about the SET, the coverage gap is about our
	// reading of it, and a composition mid-bump can honestly have both.
	return readyOutcome{reason: availableReason(v.unevaluated, v.total),
		message: withUnevaluated(withShrunkSet(msg, v.total, v.previousTotal), v.unevaluated, v.total)}
}

// readyMessageAvailable is the ONE message for the Available state, shared by Observe, Create and
// Update.
//
// It is a constant rather than three literals because setReadyCondition's idempotency guard compares
// MESSAGES: `cur.Message == message && cur.Status == cond.Status`. Three per-phase strings meant the
// guard could never fire while the phases alternated, so the condition — and its LastTransitionTime —
// was rewritten on every single reconcile (#298). A guard keyed on a value that the caller varies is
// not a guard.
//
// If you are tempted to add a phase-specific message back: the information you want is in the events
// and the logs, and per-reconcile freshness is in status.lastReconcileTime. A condition message that
// changes without the condition changing destroys the only thing LastTransitionTime is for.
const readyMessageAvailable = "Composition is up-to-date"

// ReasonInventoryNotRecorded is Ready=Unknown where status.managed was NEVER WRITTEN, as opposed to
// written-and-empty or written-and-unreadable.
//
// SPLIT OUT OF ZeroEvidence BY DIEGO'S DECISION, because one reason meant two things and a consumer
// could not tell them apart:
//
//	the recorded children are all unreadable by this controller   permanent, not ours, NOTHING TO FIX
//	the inventory was never recorded                              OUR DEFECT (#326)
//
// The distinction is load-bearing because of what reads it. installer's dependency gate allowlists
// `Unknown/ZeroEvidence` as READY, justified in their chart as "a composition with no evaluable
// children is not converging toward anything, so blocking its dependents forever would be wrong".
// That justification is sound for the first meaning and was never true of the second — and the
// installer session measured seven compositions on krateo-057 passing their gate on the strength of
// it, each holding a deployed helm release whose children were simply never recorded.
//
// NOT ALLOWLISTED ANYWHERE, deliberately, and that is why this costs installer no change: their
// gate has no else-arm, so a reason it does not know BLOCKS. A dependent built on "we never
// recorded anything" is built on nothing, and after #326's fix this state clears itself on the next
// Update — so the block is transient per composition rather than a permanent wedge.
//
// NOTE what does NOT use this reason: a composition that genuinely manages nothing. That is
// `managed: []`, present and empty, and it reports Ready=TRUE/Available (see the first branch of
// rollupManagedChildren). It has never reported ZeroEvidence, which is the part of installer's
// justification that was wrong about this code rather than merely out of date.
const ReasonInventoryNotRecorded = "InventoryNotRecorded"

// ReasonAvailablePartialCoverage is Available where the rollup assessed a MINORITY of the children
// it names. Still Ready=True — those children are unevaluated, not unhealthy — but the claim covers
// less than half of what it is about, which is a different statement from Available.
const ReasonAvailablePartialCoverage = "AvailablePartialCoverage"

// ReasonChildrenForbidden is Ready=Unknown where NOT ONE child could be assessed and at least one
// of them was REFUSED a read by RBAC (#318).
//
// A CROSS-REPO CONTRACT, matched as an exact string by installer's dependency gate
// (chart/templates/_helpers.tpl, inst.readyFromConditions) and by hack/ready-classify.sh and
// .github/scripts/check-ready-gate.py. The gate has no else-arm: Ready=True, or Unknown with a
// reason it allowlists, and everything else blocks. installer allowlisted this string BEFORE
// anything emitted it, which is the safe ordering — a reason nothing emits is never matched — and
// their test asserts that the near-miss `Unknown/Forbidden` still BLOCKS, so a misspelling fails
// visibly instead of silently claiming readiness.
//
// Do not rename it here alone, and do not "normalise" it to Forbidden. See
// reasoncontract_test.go, which pins the value for exactly this reason.
const ReasonChildrenForbidden = "ChildrenForbidden"

// availableReason distinguishes "I assessed most of this composition" from "I assessed a minority of
// it", because Ready=True means materially different things in the two cases and nothing a machine
// reads could previously tell them apart.
//
// The coverage gap is deliberate and documented (#121 tier 1): the rollup declines to invent a
// verdict for a kind whose readiness semantics it does not own, and tier 2 — a health RESTAction on
// the CompositionDefinition — is how a blueprint author supplies one. Adoption of tier 2 is 0 of 44.
//
// What was missing is not the information but its LOCATION. The count has been in the condition
// MESSAGE since #121, and a message is prose: a dependency gate, a dashboard and a post-roll check
// all read the reason and none of them parse English. So the gap was invisible to everything that
// acts on it, which is why nobody noticed how large it had become. Measured by the installer
// session, first quantification:
//
//	krateo-057  (2.13.46)  30 of 85 compositions, 396 of 725 children unevaluated  55%
//	day2        (2.13.51)  16 of 37 compositions, 631 of 790 children unevaluated  80%
//	worst: PortalAgents/portal-agents  118 of 121 unevaluated, Ready=True  97.5%
//
// Every portal Page composition is a widget container, and widgets publish `status: {}` — measured,
// 97 flexes / 139 cards / 52 buttons with ZERO conditions between them, against 152 of 152
// Deployments carrying them. So those children are not skipped by kind: krateoReady reads them and
// finds nothing to read. That is the honest reason they are unevaluated, and it is also why they
// could be fixed at the source — a widget that published a Ready condition would be assessed.
//
// THE MAJORITY LINE IS THE THRESHOLD, and it is chosen to be principled rather than tuned. Ready=True
// asserts health about the children a composition names; once most of them were not assessed, the
// assertion is about a minority of its own subject. That is a qualitative change, not a matter of
// degree, so it does not need a percentage anyone has to defend. A composition with 1 of 121
// unevaluated stays plain Available, which is right — the claim still covers what it is about.
func availableReason(unevaluated, total int) string {
	if total > 0 && unevaluated*2 > total {
		return ReasonAvailablePartialCoverage
	}
	return "Available"
}

// childHealth GETs one managed child (as the controller SA) and classifies it. Any read it cannot
// complete degrades to healthy — never to a failure — so a missing RBAC grant or a create race
// cannot regress the parent.
func (h *handler) childHealth(ctx context.Context, dyn dynamic.Interface, ref map[string]any) childState {
	apiVersion := stringField(ref, "apiVersion")
	resource := stringField(ref, "resource")
	name := stringField(ref, "name")
	namespace := stringField(ref, "namespace")
	if apiVersion == "" || resource == "" || name == "" {
		return childHealthy
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return childHealthy
	}
	gvr := gv.WithResource(resource)

	var ri dynamic.ResourceInterface = dyn.Resource(gvr)
	if namespace != "" {
		ri = dyn.Resource(gvr).Namespace(namespace)
	}

	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Rolls up as converging (never terminal) but is distinguishable, because this is also
			// what a stale status.managed looks like — see childAbsent (#261).
			return childAbsent
		}
		if apierrors.IsForbidden(err) {
			// #318. Still rolls up without failing the parent — a missing grant must never regress
			// a working composition — but it is now COUNTED and NAMED instead of vanishing into a
			// Debug line. Scoped to Forbidden alone: that is the one case that is a defect in RBAC
			// we generate, and therefore the one case an operator can act on.
			xcontext.Logger(ctx).Debug("child health: READ REFUSED by RBAC; counted unreadable, not healthy",
				"child", childID(ref), "error", err.Error())
			return childUnreadable
		}
		// No REST mapping, or a transient failure: degrade to healthy, unchanged, so a missing CRD
		// or an apiserver blip can never regress a working composition. Deliberately NOT folded
		// into childUnreadable — neither is actionable, and reporting them would make the new
		// count mean "something went wrong reading this" rather than "RBAC refused us", which is
		// the only reading that tells an operator what to fix.
		xcontext.Logger(ctx).Debug("child health: unreadable, counting healthy", "child", childID(ref), "error", err.Error())
		return childHealthy
	}
	return classifyChild(gv.Group, obj)
}

// classifyChild maps a live child to a health state with conservative, per-class predicates.
// Anything not explicitly modeled is existence-only (present == healthy).
func classifyChild(group string, obj *unstructured.Unstructured) childState {
	switch {
	case strings.HasSuffix(group, "krateo.io"):
		return krateoReady(obj) // Krateo CRs (incl. child Compositions/CompositionDefinitions)
	// Dispatch on group AND KIND, never on group alone (#256). Dispatching on group alone is how the
	// CronJob wedge happened: batch/v1 sent a CronJob to jobReady, where both branches are
	// unreachable against its schema, so every CronJob returned childConverging forever and its
	// parent sat Ready=False with it.
	//
	// The handlers below cannot detect that themselves, because they infer the kind from WHICH STATUS
	// FIELDS HAPPEN TO BE PRESENT. workloadReady reads desiredNumberScheduled to spot a DaemonSet and
	// otherwise assumes a Deployment-shaped object with spec.replicas defaulting to 1 — so any apps
	// kind carrying neither (apps/v1 ControllerRevision has no status at all) scores desired=1,
	// ready=0 and converges forever. Field-sniffing cannot distinguish "this kind has no readiness"
	// from "this kind is not ready yet", which is the whole of #256.
	//
	// So an unmodelled kind in a modelled group must fall through to the native default below rather
	// than be guessed at. That makes the NEXT such kind existence-is-health — fail-safe — instead of
	// wedging its composition until somebody writes a case for it.
	case group == "apps" && isWorkloadKind(obj.GetKind()):
		return workloadReady(obj)
	case group == "batch" && obj.GetKind() == "CronJob":
		return cronJobReady(obj)
	case group == "batch" && obj.GetKind() == "Job":
		return jobReady(obj)
	// core/v1 (group "") — only the two kinds that actually carry readiness. Everything else in the
	// core group (ConfigMap, Secret, Service, ServiceAccount, ...) is config: it has no notion of
	// being unready, so it falls through to existence-is-health below (krateo-core-provider#121).
	case group == "" && obj.GetKind() == "Pod":
		return podReady(obj)
	case group == "" && obj.GetKind() == "PersistentVolumeClaim":
		return pvcReady(obj)
	case isNativeGroup(group):
		// A built-in kind we do not model (ConfigMap, Service, RBAC, CRD, ...). These carry no
		// readiness, so existence genuinely IS health — not a gap, and not counted as one.
		return childHealthy
	default:
		// A third-party CUSTOM resource. Read only what it publishes in a documented shape
		// (thirdPartyReady); anything else stays unevaluated, never healthy-by-existence.
		// mongodbcommunity reporting `.status.phase: Failed` with no conditions lands there (#121).
		return thirdPartyReady(group, obj)
	}
}

// thirdPartyReady classifies a custom resource outside *.krateo.io and the native groups (#399).
// Until #399 every such child was childUnevaluated, so a kagent Agent at Ready=True/DeploymentReady,
// a CloudNativePG Cluster, a Keycloak and an ExternalSecret were never read, and on krateo-057 the
// model-configs composition (three kagent ModelConfigs, each Accepted=True) reported ZeroEvidence.
//
// Two arms, and only two:
//
//  1. GENERIC: a `Ready` condition, the Kubernetes convention for "this resource is serving". True
//     and current is healthy; anything else is converging. Not failed: a third party's reasons
//     (ClusterIsNotReady, SecretSyncedError, ...) are its own vocabulary, and nothing here can
//     tell a transient one from a permanent one, so this states only "not ready". krateoReady's
//     failure reasons are rest-dynamic-controller's and the runtimes', never a third party's.
//  2. KIND-SPECIFIC, only where a kind has no `Ready` but documents its readiness condition, with
//     the citation beside it (thirdPartyKindReady). `Accepted` is NEVER read generically: in
//     Gateway API, Accepted is not Programmed.
//
// No conditions, or conditions with neither, stay unevaluated.
//
// STALENESS: a True written for an older generation says nothing about the current spec, so it is
// read as converging. The condition's own observedGeneration is checked first (CloudNativePG writes
// it only there), then status.observedGeneration (Keycloak, kagent v1alpha2); when neither exists no
// gate applies, rather than refusing a resource that never publishes one.
func thirdPartyReady(group string, obj *unstructured.Unstructured) childState {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return childUnevaluated
	}
	if ready, ok := conditionByType(conds, "Ready"); ok {
		switch {
		case staleCondition(obj, ready):
			return childConverging
		case stringField(ready, "status") == "True":
			return childHealthy
		default:
			return childConverging
		}
	}
	return thirdPartyKindReady(group, obj, conds)
}

// thirdPartyKindReady holds the kind-specific arms of thirdPartyReady. Each one cites the code that
// writes the condition it reads.
func thirdPartyKindReady(group string, obj *unstructured.Unstructured, conds []any) childState {
	switch {
	case group == "kagent.dev" && obj.GetKind() == "ModelConfig":
		// kagent ModelConfig has no Ready condition. Its controller writes `Accepted`:
		//   - v1alpha2, kagent v0.7.0 go/internal/controller/reconciler/reconciler.go
		//     reconcileModelConfigStatus: Accepted=True/ModelConfigReconciled, or
		//     Accepted=False/ModelConfigReconcileFailed when the API-key Secret cannot be read.
		//     observedGeneration only top-level. This is the shape krateo-057 shows.
		//   - v1alpha3, kagent v1.0.0 go/core/internal/controller/modelconfig.go: Accepted plus
		//     ResolvedRefs (the Secret references), each with its own observedGeneration.
		// Healthy only when Accepted is True and current, and ResolvedRefs, where present, is True.
		accepted, ok := conditionByType(conds, "Accepted")
		if !ok {
			return childUnevaluated
		}
		if staleCondition(obj, accepted) || stringField(accepted, "status") != "True" {
			return childConverging
		}
		if refs, ok := conditionByType(conds, "ResolvedRefs"); ok &&
			(staleCondition(obj, refs) || stringField(refs, "status") != "True") {
			return childConverging
		}
		return childHealthy
	}
	return childUnevaluated
}

// conditionByType returns the first condition of the given type.
func conditionByType(conds []any, typ string) (map[string]any, bool) {
	for _, c := range conds {
		if cm, ok := c.(map[string]any); ok && stringField(cm, "type") == typ {
			return cm, true
		}
	}
	return nil, false
}

// staleCondition reports whether cond was written for an older generation of obj: its own
// observedGeneration if it carries one, else status.observedGeneration, else (neither published)
// never stale.
func staleCondition(obj *unstructured.Unstructured, cond map[string]any) bool {
	gen := obj.GetGeneration()
	if gen == 0 {
		return false
	}
	if og, ok := int64Field(cond, "observedGeneration"); ok {
		return og != gen
	}
	if og, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration"); found {
		return og != gen
	}
	return false
}

// int64Field reads an integer field of an unstructured map, whichever numeric type decoding left.
func int64Field(m map[string]any, key string) (int64, bool) {
	switch v := m[key].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// podReady classifies a bare Pod. Most Pods in a composition belong to a Deployment/StatefulSet and
// are judged through workloadReady instead — this is for charts that render a Pod directly.
//
// Phase is authoritative for the terminal outcomes; Running is not, because a Running Pod may still
// be failing its readiness probe and serving nothing. Note the asymmetry that keeps this safe: an
// ABSENT Ready condition is NOT treated as unhealth. That is the mistake krateoReady made (wedged
// snowplow) and jobReady made for CronJobs (wedged the portal, #112) — a field a resource does not
// carry is evidence of nothing.
func podReady(obj *unstructured.Unstructured) childState {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	switch phase {
	case "Succeeded":
		return childHealthy // ran to completion; a finished Pod is not an unhealthy one
	case "Failed":
		return childFailed // terminal: it will not recover on its own
	case "Running":
		if ready, found := podReadyCondition(obj); found {
			return boolState(ready)
		}
		return childHealthy // Running with no Ready condition: do not invent unhealth
	case "":
		return childHealthy // status not written yet, or unreadable — fail-safe, as everywhere here
	default:
		return childConverging // Pending, Unknown: not yet, but it can still get there
	}
}

// podReadyCondition returns (isTrue, found) for the Pod's Ready condition.
func podReadyCondition(obj *unstructured.Unstructured) (bool, bool) {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return false, false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || stringField(cm, "type") != "Ready" {
			continue
		}
		return stringField(cm, "status") == "True", true
	}
	return false, false
}

// pvcReady classifies a PersistentVolumeClaim by its bind phase.
//
// Pending is CONVERGING rather than failed on purpose: with the common WaitForFirstConsumer binding
// mode a claim stays Pending until a consumer Pod is scheduled, which is normal and self-resolving.
// A claim that genuinely never binds does hold the parent at Creating — correct, because a workload
// with no storage is not ready. Lost is terminal: the bound volume is gone and nothing in-cluster
// brings it back.
func pvcReady(obj *unstructured.Unstructured) childState {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	switch phase {
	case "Bound":
		return childHealthy
	case "Lost":
		return childFailed
	case "":
		return childHealthy // not yet written / unreadable — fail-safe
	default:
		return childConverging // Pending
	}
}

// krateoReady reads a Krateo child's own Ready condition. Only a Ready condition that is PRESENT
// and not-True is evidence the child is unhealthy. The absence of a Ready condition is NOT such
// evidence: many *.krateo.io CRs are leaf declarative resources that never carry conditions (e.g.
// authn's serviceaccount.authn.krateo.io ServiceAccount, a seed identity), and they are healthy by
// existence like any other non-composition child. Treating "no Ready condition" as converging
// wedged the parent Ready=False forever — snowplow never went Ready because its managed
// snowplow-seed ServiceAccount CR has no conditions. Composition/CompositionDefinition children
// always stamp a Ready condition (reason Creating/Available/Unavailable) the moment they first
// reconcile, so genuinely-not-ready nested compositions are still caught by the not-True branch.
func krateoReady(obj *unstructured.Unstructured) childState {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		// No conditions at all (authn's seed ServiceAccount, and friends). Still must NOT affect
		// Ready — that is the #72 wedge — but it is honestly unevaluated rather than assessed-healthy.
		return childUnevaluated
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || stringField(cm, "type") != "Ready" {
			continue
		}
		switch {
		case stringField(cm, "status") == "True":
			return childHealthy
		case stringField(cm, "reason") == "Unavailable":
			return childFailed
		case stringField(cm, "reason") == "CreateFailed",
			stringField(cm, "reason") == "UpdateFailed":
			// rest-dynamic-controller's reasons for a create or update that FAILED, as opposed to
			// one still in progress. Both are permanent-pending-human, so both are failures here.
			//
			// NOT SHIPPING YET, and the register matters because the arm beside this one taught me
			// that. Verified at krateo-platformops/oasgen-provider, branch feat/053-readyassessed,
			// commit 236d067, path go/rest-dynamic-controller/internal/controllers/condition/
			// condition.go: both constructors exist, Type=TypeReady, Status=False. That branch is
			// an UNMERGED DRAFT (their PR #438) targeting 0.53.0 -- not merged, not tagged, not
			// released. Contrast DriftNotConverging below, which is released and whose arm was
			// therefore fixing a live defect rather than pre-empting one.
			//
			// AND NARROWER THAN THE NAME SUGGESTS. The only permanence their author was willing to
			// establish is a POLICY REFUSAL -- PermitsURL's six refusals now wrap a sentinel, so a
			// request that was never sent is distinguishable from a vendor having a bad minute. No
			// status-code permanence rule was invented: a 500 stays transient, with a control arm
			// proving it. So this fires for "the allowlist forbids this endpoint", not for every
			// failed create, which is the right conservatism and worth knowing before anyone reads
			// the absence of this reason as evidence a create succeeded.
			//
			// MAPPED BEFORE THE REASONS SHIP, deliberately, and that is the correction to a mistake
			// in this very file. The ObserveFailed deferral used to be justified with "core-provider
			// pins v1.7.5 ... so no cluster can produce a reason this rollup does not understand" —
			// false the day it was written, because the rollup reads CHILDREN and a child's reason
			// comes from the child's own controller, on the child's own release schedule
			// (core-provider#320). Waiting for the reason to appear is waiting for the defect.
			//
			// An unmapped reason falls to the converging default, which is the inversion this whole
			// issue is about: the reason invented to say "this will not progress" would report as
			// "still working on it". Adding the case early costs nothing if the reason never
			// arrives.
			//
			// WHY THE VERB IS IN THE REASON AT ALL, since it was previously in the message: the
			// runtime sets Creating on BOTH create outcomes and the distinction lived in prose
			// ("message=create failed: 422"). The portal cannot branch on free text that is ours to
			// reword, which was the frontend session's objection and the reason oasgen-provider
			// moved the verb into the reason instead (their #42/#433 resolution).
			//
			// NO DeleteFailed, and its absence is a decision rather than an omission. A refused
			// delete must HOLD the finalizer (#261): giving it a terminal-looking Ready reason
			// invites exactly the finalizer removal that orphans a paid resource. If a
			// `DeleteFailed` ever appears, it does not belong in this arm.
			return childFailed
		case stringField(cm, "reason") == "AuthorizationDenied":
			// rest-dynamic-controller's reason for a refusal that WILL NOT BE RETRIED — a 403 with
			// no rate-limit signal is a decision, and nothing the controller does next can change
			// it (oasgen-provider#313). So it is a failure, not a convergence.
			//
			// Without this arm it fell to the default and the parent reported Ready=False/Creating:
			// THE CONDITION BUILT SPECIFICALLY TO SAY "STOP, THIS IS PERMANENT" READ UPSTREAM AS
			// "STILL WORKING ON IT", indefinitely. Found by the oasgen-provider session reading
			// what RDC publishes against this mapping (their #436, core-provider#320).
			//
			// AND NOTE WHY IT WAS MISSED, because the next specific reason will arrive the same
			// way: a GENERIC `Unavailable` would have bucketed correctly here all along. The more
			// precise RDC made the child's reason, the less this rollup understood it. Specificity
			// in a child's reason is a cost paid by every consumer that matches on reasons, and
			// nothing in either repo makes that visible — the trip-wire in
			// reasoncontract_test.go is the only thing that can.
			//
			// Keyed on the reason, not on Status=False, deliberately: a converging child also
			// reports False and must keep holding the parent back rather than failing it.
			return childFailed
		case stringField(cm, "reason") == "DriftNotConverging":
			// THE ONLY ONE OF THESE THAT WAS WRONG ABOUT A LIVE CLUSTER. Unlike CreateFailed and
			// UpdateFailed above, which are mapped ahead of their arrival, DriftNotConverging has
			// shipped since oasgen-provider 0.33.0 (now 0.50.0) and every composition carrying a
			// child in that state has been reported as CONVERGING by this rollup the whole time.
			//
			// Verified first-hand rather than taken on report, at
			// krateo-platformops/oasgen-provider, go/rest-dynamic-controller/internal/controllers/
			// condition/condition.go:109-128 -- Type=TypeReady, Status=False,
			// Reason=ReasonDriftNotConverging. Introduced by 156bc83 (2026-10-26, their #216),
			// which `compare` puts BEHIND tag 0.33.0 by 5 commits and AHEAD of 0.32.0 by 5, so
			// 0.33.0 is the first tag that carries it.
			//
			// Terminal by construction, in the constructor's own words: "a difference has been
			// reported repeatedly and the update issued for it has not resolved it, so the
			// controller has stopped issuing that update ... nothing is retryable either, because
			// the one action available has been tried and changed nothing. The remedy is a
			// declaration change ... which is a human's decision, so the condition's job is to say
			// which field and stop."
			//
			// Same shape as AuthorizationDenied, and found the same way: the more precisely RDC
			// named the state, the less this rollup understood it. A generic Unavailable would have
			// bucketed correctly all along.
			return childFailed
		case stringField(cm, "reason") == condition.ReasonZeroEvidence,
			stringField(cm, "reason") == "NotObserved",
			stringField(cm, "reason") == "ObserveFailed":
			// NotObserved is PROVIDER-RUNTIME's reason, and only provider-runtime's. Confirmed at
			// krateo-platformops/provider-runtime 499ea69: markNotObserved (pkg/reconciler/
			// ready.go:60) writes prv1.NotObserved() from reconciler.go:869 (Connect failed) and
			// :909 (Observe failed), unless the provider already assessed Ready itself.
			// rest-dynamic-controller has a NotObserved() constructor too and it is NOT this: it
			// is Type=Observed, Status=False, Reason=NoResponseBody -- a different condition type
			// that this Ready-based rollup never reads. Two repos, one constructor name, different
			// objects; the collision is why this says which repo.
			//
			// A LITERAL, NOT condition.ReasonObserveFailed, and the reason is the point of #320.
			// The const does not exist in the unstructured-runtime version the cdc pins (it was
			// added later), and even once it does, importing it would be WRONG here: these three
			// reasons arrive on CHILDREN, written by the children's own controllers on their own
			// release schedules, so a const resolves to OUR pin's value while the string on the
			// object is THEIRS. Tying a child's reason to our pin is exactly the inference this
			// file's ObserveFailed deferral got wrong. Contrast ReasonZeroEvidence above, which the
			// cdc EMITS about itself — there the const is right, and reasoncontract_test.go pins
			// its value because a third repo matches on it.
			//
			// ObserveFailed JOINS IT HERE, and that is a decision Diego took rather than a
			// tidy-up. The two reasons are THE SAME CLAIM from two runtimes: provider-runtime
			// writes NotObserved when a provider's Connect or Observe returns an error;
			// unstructured-runtime writes ObserveFailed when a handler's Observe returns one.
			// Bucketing them differently meant one situation got two verdicts, decided by which
			// runtime the provider happened to be built on — which no consumer of this rollup can
			// see. Confirmed by the provider-runtime session against their own call sites, who
			// also corrected my belief that NotObserved meant "the runtime never got an answer":
			// it does not, it is the provider's error path.
			//
			// UNEVALUATED AND NOT CONVERGING, of the two uniform answers, because converging is
			// the one that states something false. Converging asserts progress; nothing is
			// progressing. Unknown asserts nothing, which is exactly what is known. The earlier
			// reasoning for leaving ObserveFailed on the converging default — that it does not
			// say transient or persistent — argues for this bucket rather than against it.
			//
			// THE COST, NAMED: a persistently unobservable child now passes installer's dependency
			// gate, because an all-unevaluated parent reports ZeroEvidence and the gate accepts
			// exactly that. The honest fix is NOT to mis-state the child's health to force the
			// gate's hand — it is that the parent cannot currently say WHY it has no evidence, so
			// "no children recorded" and "children nobody could read" are indistinguishable to the
			// gate. That capability is #318 (unreadableChildrenCount plus a distinct reason), and
			// the unobservable case belongs to the same family as the forbidden one. Until it
			// lands, the two runtimes at least agree.
			//
			// Both mean "the external resource could not be observed" — an Observe or Connect
			// failure, with the raw error left on Synced. They
			// belongs HERE and not in the converging branch below for the same reason ZeroEvidence
			// does, and the distinction is the whole point: the child is not reporting that it is
			// unhealthy or that it is still coming up. It is reporting that NOTHING ASSESSED IT.
			//
			// Without this arm it falls to the default and the parent reports Ready=False/Creating,
			// so an Observe outage on one child (a CompositionDefinition, Alert, RestDefinition,
			// Repo or LocalResource) makes the parent look like it is converging when nothing is.
			// installer's depsReady would then hold back first creation of every dependent, and
			// verify-roll would withhold its verdict — an apiserver blip on one child stalling an
			// install is exactly the #165 shape that ZeroEvidence exists to prevent.
			//
			// Keyed on the REASON, never on bare Unknown, for the same reason as ZeroEvidence: a
			// genuinely converging child also reports a non-True Ready and must keep holding the
			// parent back.
			//
			// The child says its own health is UNKNOWN, not bad (#165). Propagate ignorance as
			// ignorance: counting it converging would hold this parent Ready=False for as long as
			// the child stays unevaluable, which is indefinitely.
			//
			// This is load-bearing, not symmetry. On krateo-057 the Installer composition lists
			// modelconfigs/model-configs among its managed children, and model-configs is one of
			// the two compositions at zero evidence. Without this branch, emitting the condition
			// would put the Installer composition — the root of the install — at Ready=False
			// permanently the moment it shipped.
			//
			// Keyed on the REASON, never on bare Unknown: a genuinely converging child also
			// reports a non-True Ready and must keep holding the parent back. That transient is
			// what the converging branch exists for.
			return childUnevaluated
		default:
			// Not-True for any other reason: converging. TWO routes reach here and both must,
			// so do not narrow this to a Creating-only case.
			//
			//  1. A child the cdc itself judged not-ready-yet (reason Creating from the rollup).
			//  2. A child whose BLUEPRINT declared it converging — .status.health.ready=false with
			//     reason Creating (#234). This is the route that needs no code of its own: because
			//     resolveReady stamps reason Creating instead of Unavailable, such a child misses
			//     the childFailed arm above and lands here. That is the whole of "krateoReady stops
			//     mapping the converging case to childFailed" — the mapping is keyed on the reason,
			//     and #234 changed the reason rather than adding a branch.
			return childConverging
		}
	}
	// Has conditions but no Ready type. This is publish-pet's case: github.krateo.io/repositories
	// carries only Synced=False/ReconcileError, so there is nothing here to read. Unevaluated, not
	// healthy — the composition was reporting green over a child whose failure we never looked at.
	return childUnevaluated
}

// workloadReady handles Deployment/StatefulSet/ReplicaSet (spec.replicas) and DaemonSet
// (desiredNumberScheduled). A workload with unready replicas — including a CrashLoopBackOff pod that
// holds readyReplicas at 0 — is CONVERGING, never failed (it self-progresses), so the composition
// stays Ready=False (Creating) until the workload actually serves rather than merely being applied
// (krateo-core-provider#96).
// isWorkloadKind lists the apps/v1 kinds whose readiness workloadReady actually models. The group's
// remaining kind, ControllerRevision, carries no status and therefore no readiness — it falls
// through to existence-is-health rather than being judged by a handler that would read its missing
// replica counts as "0 of 1 ready".
//
// A list rather than a negation: a new apps kind should arrive as unmodelled-and-healthy, which is
// recoverable, instead of as silently-converging, which wedges a composition. Adding a kind here is
// a deliberate claim that workloadReady reads it correctly.
func isWorkloadKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet":
		return true
	}
	return false
}

// workloadReady classifies an apps/v1 workload by its replica counts. It is reached only for the
// kinds isWorkloadKind admits, because it identifies the kind by WHICH STATUS FIELDS ARE PRESENT
// rather than by the kind itself: a DaemonSet is recognised by status.desiredNumberScheduled, and
// anything else is assumed Deployment-shaped. That inference is sound for the four kinds it models
// and silently wrong for any other, which is why the caller gates it (#256).
func workloadReady(obj *unstructured.Unstructured) childState {
	// DaemonSet: readiness is per-node (desiredNumberScheduled vs numberReady); numberUnavailable>0
	// means some node's pod is not ready (e.g. crashing).
	if desired, found, _ := unstructured.NestedInt64(obj.Object, "status", "desiredNumberScheduled"); found {
		ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberReady")
		unavailable, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberUnavailable")
		return boolState(ready >= desired && unavailable == 0)
	}
	// Deployment/StatefulSet/ReplicaSet: spec.replicas defaults to 1 when unset (Kubernetes' own
	// default), so an omitted replicas must NOT read as desired=0 — which would call a 0-ready
	// crashing workload "healthy". status.unavailableReplicas>0 is the direct "not serving" signal.
	desired := int64(1)
	if d, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); found {
		desired = d
	}
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	unavailable, _, _ := unstructured.NestedInt64(obj.Object, "status", "unavailableReplicas")
	return boolState(ready >= desired && unavailable == 0)
}

// cronJobReady: a CronJob is a SCHEDULE, not a unit of work, so it has nothing to complete and
// "converging" never ends. It is healthy once it exists and is not suspended.
//
// It must not be judged by jobReady. A CronJob is batch/v1, so dispatching on group alone sent it
// there, where BOTH branches are unreachable against the CronJob schema: CronJobStatus carries only
// {active, lastScheduleTime, lastSuccessfulTime} so status.succeeded never appears and the healthy
// branch cannot be taken, and backoffLimit lives at spec.jobTemplate.spec.backoffLimit rather than
// spec.backoffLimit so the failed branch cannot be taken either. Every CronJob therefore returned
// childConverging forever, and its parent composition sat Ready=False forever with it.
//
// Observed: krateo-057's Frontend composition went Ready=False "1 of 13 managed children are not
// ready" the moment frontend 1.6.18 added a preview-sandbox janitor CronJob, and stayed there
// across two successful firings — lastScheduleTime and lastSuccessfulTime are fields jobReady never
// reads. The blast radius was not cosmetic: the installer gates rendering on inst.depsReady, so a
// permanently-not-Ready Frontend stopped the Portal composition from ever being re-created after a
// pin flip removed it, which froze the portal Helm release, which froze every chart that release
// registers. One unreachable predicate, four layers of consequence.
//
// This is the same shape as the krateoReady note below — a predicate treating "the field I look for
// is absent" as evidence of unhealth, when it is only evidence that the field does not exist on
// that kind. Suspension is the one genuine not-ready state a CronJob has, so it is the only thing
// worth testing.
// A SUSPENDED CronJob is healthy, not converging. childConverging means "not ready YET, keep
// waiting" — and a suspended CronJob never stops being suspended on its own, so returning it would
// pin the parent composition Ready=False forever. That is the same permanent-wait trap this function
// exists to fix, just narrower: a chart that ships `suspend: true` (a janitor you enable later is a
// normal pattern) would wedge its composition exactly the way the unreachable jobReady branches did.
//
// Suspension is also not unhealth: the object matches its declared spec, which is what Ready means
// for a child. "Nobody asked it to run" is a statement about intent, not about failure. If a
// suspended schedule ever needs surfacing it belongs in a status projection, not in a health
// predicate that blocks a dependency gate.
// The parameter is deliberately unused: existence IS the verdict. It is kept for signature parity
// with the other predicates so the classifyChild dispatch stays uniform.
func cronJobReady(_ *unstructured.Unstructured) childState {
	return childHealthy
}

// jobReady: succeeded -> healthy; failed beyond backoffLimit -> failed; else converging.
// Jobs ONLY — see cronJobReady for why a CronJob must never reach this.
func jobReady(obj *unstructured.Unstructured) childState {
	if succeeded, _, _ := unstructured.NestedInt64(obj.Object, "status", "succeeded"); succeeded > 0 {
		return childHealthy
	}
	failed, _, _ := unstructured.NestedInt64(obj.Object, "status", "failed")
	if backoff, found, _ := unstructured.NestedInt64(obj.Object, "spec", "backoffLimit"); found && failed > backoff {
		return childFailed
	}
	return childConverging
}

func boolState(ok bool) childState {
	if ok {
		return childHealthy
	}
	return childConverging
}

func childID(ref map[string]any) string {
	kind, name, ns := stringField(ref, "resource"), stringField(ref, "name"), stringField(ref, "namespace")
	if ns != "" {
		return fmt.Sprintf("%s/%s/%s", kind, ns, name)
	}
	return fmt.Sprintf("%s/%s", kind, name)
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func joinCap(items []string, cap int) string {
	if len(items) <= cap {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(items[:cap], ", "), len(items)-cap)
}

// coverageScalars is the #318 numbers a verdict publishes, as field name to value.
//
// SEPARATED FROM THE WRITE, and the separation was forced by a surviving mutant rather than
// chosen: dropping `- len(v.unreadable)` from the arithmetic compiled and broke nothing, because
// the test that covered it RECOMPUTED the arithmetic instead of reading what was written. A test
// that reconstructs the thing under test can only ever agree with itself. The write needs a
// kubeconfig and a dynamic client; the arithmetic does not, so the arithmetic lives here where a
// test can address it directly.
//
// Returned as a map keyed by field name so the names are in one place: three numbers written at one
// site, declared reserved in core-provider's statusfields.go, and declared in its
// statics/status.schema.json. A field missing from the schema is pruned by the apiserver with no
// error at any layer a consumer can see.
func coverageScalars(v healthVerdict) map[string]int {
	evaluated := v.total - v.unevaluated - len(v.unreadable)
	if evaluated < 0 {
		// Cannot happen: every child lands in exactly one bucket and only these two are
		// non-evaluating. Clamped rather than trusted because a negative count published to a
		// dashboard is worse than a wrong one — it is unreadable, and it would be the first
		// evidence that a new childState was added without being accounted for here.
		evaluated = 0
	}
	return map[string]int{
		"managedChildrenCount":    v.total,
		"healthEvaluatedChildren": evaluated,
		"unreadableChildrenCount": len(v.unreadable),
	}
}
