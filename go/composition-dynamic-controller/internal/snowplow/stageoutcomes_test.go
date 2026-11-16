package snowplow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// Every form snowplow's contract names, and the forms it says must never read as success
// (docs/howto-frontend-call-dryrun-raw.md section 5: "only the array [] means every stage
// succeeded").
func TestParseStageOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		values    []string // nil: header absent
		complete  bool
		known     bool
		failed    []StageOutcome
		truncated int // -1: not truncated
	}{
		{"absent: unknown, never success", nil, false, false, nil, -1},
		{"[]: every stage succeeded", []string{"[]"}, true, true, nil, -1},
		{"[] with surrounding space", []string{" [] "}, true, true, nil, -1},
		{"one failed stage", []string{`[{"name":"bundle","ok":false,"reason":"NotFound"}]`}, false, true,
			[]StageOutcome{{Name: "bundle", Reason: "NotFound"}}, -1},
		{"a skipped write names the stage it depends on", []string{`[{"name":"read","ok":false,"reason":"Forbidden"},{"name":"write","ok":false,"reason":"Skipped","dependsOn":"read"}]`}, false, true,
			[]StageOutcome{{Name: "read", Reason: "Forbidden"}, {Name: "write", Reason: "Skipped", DependsOn: "read"}}, -1},
		{"an older snowplow's ok:true rows are not failures", []string{`[{"name":"a","ok":true},{"name":"b","ok":true}]`}, true, true, nil, -1},
		{"ok:true rows dropped, ok:false kept", []string{`[{"name":"a","ok":true},{"name":"b","ok":false,"reason":"Timeout"}]`}, false, true,
			[]StageOutcome{{Name: "b", Reason: "Timeout"}}, -1},
		{"an unknown code is carried verbatim", []string{`[{"name":"x","ok":false,"reason":"SomethingNew"}]`}, false, true,
			[]StageOutcome{{Name: "x", Reason: "SomethingNew"}}, -1},
		{"the resolve-level row", []string{`[{"name":"","ok":false,"reason":"Aborted"},{"name":"a","ok":false,"reason":"NotRun"}]`}, false, true,
			[]StageOutcome{{Name: "", Reason: "Aborted"}, {Name: "a", Reason: "NotRun"}}, -1},
		{"truncated: incomplete, with snowplow's count", []string{`{"truncated":true,"failed":57}`}, false, true, nil, 57},
		{"an object that is not the truncated form", []string{`{"truncated":false,"failed":2}`}, false, false, nil, -1},
		{"an object with no count", []string{`{"truncated":true}`}, false, false, nil, -1},
		{"not JSON", []string{`ok`}, false, false, nil, -1},
		{"null is not []", []string{`null`}, false, false, nil, -1},
		{"empty value", []string{``}, false, false, nil, -1},
		{"sent twice", []string{`[]`, `[]`}, false, false, nil, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range c.values {
				h.Add(StageOutcomesHeader, v)
			}
			got := ParseStageOutcomes(h)
			if got.Complete() != c.complete || got.Known() != c.known {
				t.Fatalf("Complete=%v Known=%v, want %v %v (%+v)", got.Complete(), got.Known(), c.complete, c.known, got)
			}
			if got.Present != (c.values != nil) {
				t.Errorf("Present = %v", got.Present)
			}
			if !reflect.DeepEqual(got.Failed, c.failed) {
				t.Errorf("Failed = %+v, want %+v", got.Failed, c.failed)
			}
			if (c.truncated >= 0) != got.Truncated || (got.Truncated && got.TruncatedFailed != c.truncated) {
				t.Errorf("Truncated = %v/%d, want %d", got.Truncated, got.TruncatedFailed, c.truncated)
			}
			if c.known && !c.complete && got.FailedCount() == 0 {
				t.Errorf("an incomplete verdict counts no failed stage")
			}
		})
	}
}

func TestStageOutcomesSummary(t *testing.T) {
	o := StageOutcomes{Present: true, Failed: []StageOutcome{
		{Name: "", Reason: "Aborted"}, {Name: "read", Reason: "Forbidden"}, {Name: "write", Reason: "Skipped", DependsOn: "read"}, {Name: "odd"},
	}}
	if got, want := o.Summary(), "(resolve): Aborted, read: Forbidden, write: Skipped (dependsOn read), odd: (no reason)"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	tr := StageOutcomes{Present: true, Truncated: true, TruncatedFailed: 57}
	if got, want := tr.Summary(), "57 stages (snowplow truncated the list)"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}

// Resolve carries the header from the reply to the caller, next to the data, on a partial 200
// (the case authn measured: a missing Bundle, the body partial, the reply still 200).
func TestResolveCarriesTheStageOutcomes(t *testing.T) {
	for _, c := range []struct {
		name   string
		header string // "": not sent
		want   func(StageOutcomes) bool
	}{
		{"[]", "[]", StageOutcomes.Complete},
		{"partial", `[{"name":"bundle","ok":false,"reason":"NotFound"}]`, func(o StageOutcomes) bool { return o.Known() && o.FailedCount() == 1 }},
		{"absent", "", func(o StageOutcomes) bool { return !o.Present }},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.header != "" {
					w.Header().Set(StageOutcomesHeader, c.header)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"url": map[string]any{"value": "https://x"}}})
			}))
			defer srv.Close()
			res, err := New(srv.URL, nil).Resolve(context.Background(), ApiRef{Name: "n", Namespace: "ns"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !c.want(res.Outcomes) {
				t.Errorf("Outcomes = %+v", res.Outcomes)
			}
			if _, ok := res.API["url"]; !ok {
				t.Errorf("the data that resolved was dropped: %v", res.API)
			}
		})
	}
}
