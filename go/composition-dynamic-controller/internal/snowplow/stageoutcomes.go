package snowplow

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// StageOutcomesHeader is snowplow's per-stage verdict on a RESTAction /call reply (snowplow#904,
// on every RESTAction reply since 1.12.57). It is the only place a partial resolve shows: a stage
// with continueOnError can fail and the reply is still a 200 with that stage's data missing.
//
// The contract, from snowplow's docs/howto-frontend-call-dryrun-raw.md section 5:
//   - a compact JSON array of the stages that did NOT succeed; `[]` means every stage succeeded;
//   - above 4 KiB it is `{"truncated":true,"failed":N}` instead, to be read as incomplete;
//   - "A parser must never read a parse failure, a non-array or a missing header as success:
//     only the array `[]` means every stage succeeded."
//
// The reason codes are a closed set in snowplow today that may grow, so a code is carried
// verbatim and never interpreted here.
const StageOutcomesHeader = "X-Snowplow-Stage-Outcomes"

// StageOutcome is one row: a stage that did not succeed. A resolve-level row has Name "" and
// Reason "Aborted". DependsOn is set only on a "Skipped" row and names the stage that failed.
type StageOutcome struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	Reason    string `json:"reason,omitempty"`
	DependsOn string `json:"dependsOn,omitempty"`
}

// StageOutcomes is what the header said, or that it said nothing usable.
type StageOutcomes struct {
	// Present is whether the header was sent at all.
	Present bool
	// Unreadable is set when the header was sent but is neither an array nor the truncated
	// object; it says why. Never success.
	Unreadable string
	// Failed lists the stages that did not succeed, in snowplow's (topological) order.
	Failed []StageOutcome
	// Truncated means snowplow replaced the rows with a count: TruncatedFailed stages did not
	// succeed and which ones is unknown.
	Truncated       bool
	TruncatedFailed int
}

// Complete reports whether snowplow confirmed that every stage succeeded: the header was sent,
// and it is exactly an array with no failed row.
func (s StageOutcomes) Complete() bool {
	return s.Present && s.Unreadable == "" && !s.Truncated && len(s.Failed) == 0
}

// Known reports whether the header said something readable, complete or not.
func (s StageOutcomes) Known() bool { return s.Present && s.Unreadable == "" }

// FailedCount is how many stages did not succeed: the rows, or snowplow's count when truncated.
func (s StageOutcomes) FailedCount() int {
	if s.Truncated {
		return s.TruncatedFailed
	}
	return len(s.Failed)
}

// Summary names each failed stage and its code, e.g. `bundle: NotFound, publish: Skipped (dependsOn
// bundle)`, in snowplow's order. A resolve-level row reads `(resolve): Aborted`.
func (s StageOutcomes) Summary() string {
	if s.Truncated {
		return fmt.Sprintf("%d stages (snowplow truncated the list)", s.TruncatedFailed)
	}
	parts := make([]string, 0, len(s.Failed))
	for _, f := range s.Failed {
		name := f.Name
		if name == "" {
			name = "(resolve)"
		}
		reason := f.Reason
		if reason == "" {
			reason = "(no reason)"
		}
		p := name + ": " + reason
		if f.DependsOn != "" {
			p += " (dependsOn " + f.DependsOn + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// ParseStageOutcomes reads the header from a reply's headers. A row with ok:true (an older
// snowplow listed succeeded stages too) is not a failure and is dropped.
func ParseStageOutcomes(h http.Header) StageOutcomes {
	vals := h.Values(StageOutcomesHeader)
	if len(vals) == 0 {
		return StageOutcomes{}
	}
	out := StageOutcomes{Present: true}
	if len(vals) > 1 {
		out.Unreadable = fmt.Sprintf("the header was sent %d times", len(vals))
		return out
	}
	raw := strings.TrimSpace(vals[0])
	if strings.HasPrefix(raw, "{") {
		var t struct {
			Truncated *bool `json:"truncated"`
			Failed    *int  `json:"failed"`
		}
		if err := json.Unmarshal([]byte(raw), &t); err != nil || t.Truncated == nil || !*t.Truncated || t.Failed == nil || *t.Failed < 0 {
			out.Unreadable = "the header is an object but not snowplow's truncated form"
			return out
		}
		out.Truncated, out.TruncatedFailed = true, *t.Failed
		return out
	}
	var rows []StageOutcome
	if err := json.Unmarshal([]byte(raw), &rows); err != nil || rows == nil {
		out.Unreadable = "the header is not a JSON array"
		return out
	}
	for _, r := range rows {
		if !r.OK {
			out.Failed = append(out.Failed, r)
		}
	}
	return out
}
