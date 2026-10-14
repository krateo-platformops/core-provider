// The composition-version policy has TWO sources of truth — this package, and the chart templates
// that install it — and nothing compared them until this test.
//
// That is how #173 happened. The #66 fix (JSONPatch instead of ApplyConfiguration, so that a
// Quantity string in an unrelated field cannot deny a composition write) changed objects() and left
// both charts on the broken form. Every cluster takes its policy from a chart, so the fix reached
// nobody: krateo-057 was still carrying the pre-#66 mutation months later, verbatim, after an
// upgrade to a release whose notes said #66 was fixed.
//
// Equality here is STRUCTURAL, not "equivalent CEL", and that is deliberate. Two expressions can be
// semantically equal and still differ on whether `add` to /metadata/labels/<key> succeeds when
// /metadata/labels is absent — which is the entire reason objects() uses a two-branch expression. A
// test that accepted any CEL computing the same label would pass that bug straight through.
//
// It compares the whole spec rather than only the mutation. `operations: ["CREATE", "UPDATE"]` is
// load-bearing (the cdc ListWatcher filters on exact equality of the label this stamps, so a
// CREATE-only chart would strand every bumped CR), and `failurePolicy: Fail` means a wrong
// matchConstraints denies writes instead of skipping them. There is no field here where chart and
// code may legitimately disagree.
package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// repoRootFromPackage inverts this package's path within the repository. Tests run with the package
// directory as the working directory.
const repoRootFromPackage = "../../../../.."

// chartPolicies are every chart that ships this policy. Both are PUBLISHED artifacts, so a fix that
// lands in one and not the other leaves clusters disagreeing about the policy's form depending on
// which chart installed it — worse than the uniform-broken state. Adding a third chart that
// installs the policy means adding it here.
var chartPolicies = []struct {
	chart string // published chart name, which is not always the directory name
	path  string // repository-relative
}{
	{"core-provider", "helm/core-provider/templates/compositions-version-policy.yaml"},
	{"core-provider-target", "helm/target-chart/templates/compositions-version-policy.yaml"},
}

// helmCommentBlock matches a {{/* ... */}} template comment.
var helmCommentBlock = regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)

// loadChartDocuments returns the YAML documents of a policy template, parsed as plain YAML.
//
// These templates carry no Go-template actions outside their header comment — the policy is a fixed
// cluster singleton with nothing to interpolate — so they can be read without rendering, which keeps
// this test free of a helm binary. That is an assumption about the file, so it is asserted rather
// than relied on: if someone templates part of the policy, this fails with an explanation instead of
// quietly comparing a `{{ ... }}` string against a rendered value.
func loadChartDocuments(t *testing.T, path string) []map[string]any {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	body := helmCommentBlock.ReplaceAllString(string(raw), "")
	if strings.Contains(body, "{{") {
		t.Fatalf("%s now contains Go-template actions outside its {{/* */}} header. This test parses "+
			"the template as plain YAML, which is only valid while the policy is a fixed singleton. "+
			"Teach it to render the chart (helm template) before comparing, or it will compare an "+
			"unrendered string against a value and pass for the wrong reason.", path)
	}

	var out []map[string]any
	for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(body, -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("parsing a document of %s: %v", path, err)
		}
		out = append(out, m)
	}
	return out
}

// documentOfKind returns the single document of the given kind.
func documentOfKind(t *testing.T, docs []map[string]any, path, kind string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, d := range docs {
		if k, _ := d["kind"].(string); k == kind {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s holds %d documents of kind %s, want exactly 1", path, len(found), kind)
	}
	return found[0]
}

// pretty renders a value the way a reader can diff it.
func pretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}

func TestChartPolicyMatchesGeneratedPolicy(t *testing.T) {
	root, err := filepath.Abs(repoRootFromPackage)
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}

	wantPolicy, wantBinding := objects()

	for _, c := range chartPolicies {
		t.Run(c.chart, func(t *testing.T) {
			full := filepath.Join(root, filepath.FromSlash(c.path))
			if _, err := os.Stat(full); err != nil {
				t.Fatalf("%s is missing (%v). If this chart no longer ships the policy, remove it "+
					"from chartPolicies deliberately rather than leaving the check to pass on "+
					"nothing.", c.path, err)
			}
			docs := loadChartDocuments(t, full)

			for _, tc := range []struct {
				kind string
				want *unstructured.Unstructured
			}{
				{"MutatingAdmissionPolicy", wantPolicy},
				{"MutatingAdmissionPolicyBinding", wantBinding},
			} {
				got := documentOfKind(t, docs, c.path, tc.kind)

				if v, _ := got["apiVersion"].(string); v != tc.want.GetAPIVersion() {
					t.Errorf("%s: %s apiVersion is %q, want %q", c.path, tc.kind, v, tc.want.GetAPIVersion())
				}
				name, _ := got["metadata"].(map[string]any)["name"].(string)
				if name != tc.want.GetName() {
					t.Errorf("%s: %s is named %q, want the cluster singleton %q — core-provider "+
						"projects and migrates the policy by this exact name",
						c.path, tc.kind, name, tc.want.GetName())
				}

				if !reflect.DeepEqual(got["spec"], tc.want.Object["spec"]) {
					t.Errorf("%s: the %s spec differs from policy.objects().\n\nchart:\n%s\n\ncode:\n%s\n\n"+
						"These must stay identical. The chart is what every cluster actually installs, so "+
						"a fix applied only to the Go side reaches nobody — that is #173, where #66's "+
						"JSONPatch fix sat unshipped while clusters kept the ApplyConfiguration mutation "+
						"that denies a write whenever an unrelated field holds a Quantity string.",
						c.path, tc.kind, pretty(got["spec"]), pretty(tc.want.Object["spec"]))
				}
			}
		})
	}
}

// The pre-#66 mutation must not reappear in a chart. The parity test above would already catch it,
// but only as a spec diff a reader has to interpret; this names the specific regression and the
// reason it is dangerous, so the failure explains itself.
func TestChartsDoNotShipTheLegacyApplyConfiguration(t *testing.T) {
	root, err := filepath.Abs(repoRootFromPackage)
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}

	for _, c := range chartPolicies {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(c.path)))
		if err != nil {
			t.Fatalf("reading %s: %v", c.path, err)
		}
		if strings.Contains(string(raw), legacyApplyConfigurationExpression()) {
			t.Errorf("%s ships the pre-#66 ApplyConfiguration expression. ApplyConfiguration converts "+
				"the WHOLE incoming object to its typed form before merging, so a Quantity string like "+
				"\"200m\" under a schema that types the field numeric fails conversion — and with "+
				"failurePolicy: Fail the composition write is DENIED, which wedges the umbrella "+
				"reconcile that re-applies its manifest every cycle (#66, #173).", c.path)
		}
	}
}
