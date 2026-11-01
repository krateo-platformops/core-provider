package composition

import (
	"context"
	"fmt"
	"strings"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
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
		return healthVerdict{ready: true, reason: "Available", message: "Composition is up-to-date"}
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
			reason: "ZeroEvidence",
			message: "Composition health is unknown: no managed children are recorded, so none " +
				"could be health-evaluated",
		}
	}

	var failing, converging []string
	unevaluated := 0
	var absent []string
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
		}
	}

	switch {
	case len(failing) > 0:
		return healthVerdict{
			absent:  absent,
			ready:   false,
			reason:  "Unavailable",
			message: fmt.Sprintf("managed children not healthy: %s", joinCap(failing, 3)),
			failing: failing,
			total:   len(managed),
		}
	case len(converging) > 0:
		return healthVerdict{
			ready:  false,
			reason: "Creating",
			message: fmt.Sprintf("%d of %d managed children are not ready: %s",
				len(converging), len(managed), joinCap(converging, 3)),
			converging: converging,
			absent:     absent,
			total:      len(managed),
		}
	case unevaluated == len(managed):
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
		return healthVerdict{
			ready: false, reason: "ZeroEvidence",
			message:     zeroEvidenceMessage(len(managed)),
			unevaluated: unevaluated,
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
			total:       len(managed),
		}
	}
}

// zeroEvidenceMessage says what is and is not known, without implying the composition is broken.
// It must not read as a failure: an operator seeing it has a coverage gap, not an outage.
func zeroEvidenceMessage(total int) string {
	return fmt.Sprintf("Composition health is unknown: none of its %d managed children could be "+
		"health-evaluated (no readiness semantics this controller knows how to read)", total)
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
func resolveReady(p projectedHealth, defaultMsg string, v healthVerdict) readyOutcome {
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
	case "ZeroEvidence":
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
		return readyOutcome{reason: "ZeroEvidence", message: v.message}
	}
	// Available. The base is the phase message the caller supplied ("Composition is up-to-date",
	// "Composition values updated"), or the author's projected message when they wrote one — NOT
	// v.message, which hardcodes "Composition is up-to-date" and would overwrite the phase.
	msg := defaultMsg
	if p.present && p.message != "" {
		msg = p.message
	}
	// Both caveats, innermost first: the shrink is about the SET, the coverage gap is about our
	// reading of it, and a composition mid-bump can honestly have both.
	return readyOutcome{reason: availableReason(v.unevaluated, v.total),
		message: withUnevaluated(withShrunkSet(msg, v.total, v.previousTotal), v.unevaluated, v.total)}
}

// ReasonAvailablePartialCoverage is Available where the rollup assessed a MINORITY of the children
// it names. Still Ready=True — those children are unevaluated, not unhealthy — but the claim covers
// less than half of what it is about, which is a different statement from Available.
const ReasonAvailablePartialCoverage = "AvailablePartialCoverage"

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
		// Forbidden (SA lacks read), no REST mapping, transient: degrade to healthy so a missing
		// grant can never regress a working composition.
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
		// A CUSTOM resource nobody told us how to read. Not healthy-by-existence — unevaluated.
		// mongodbcommunity reporting `.status.phase: Failed` with no conditions lands here (#121).
		return childUnevaluated
	}
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
		case stringField(cm, "reason") == "ZeroEvidence":
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
