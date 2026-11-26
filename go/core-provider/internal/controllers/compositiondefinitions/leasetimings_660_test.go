package compositiondefinitions

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

// #660: the ownership Lease timings are ONE chart value, cdc.ownershipLease, rendered under the same three
// names into core-provider's own ConfigMap and into every controller's (the dedicated and the pool process
// env, otelKeyLines checks they agree), once each. Then core-provider's ConfigMap, as rendered, is read
// through the reader main uses, so the key the chart renders is the key the binary reads. The
// composition-dynamic-controller holds its own reader to the same render (its TestChartRendersTheTimingsItReads).
// Every name and value below is a literal.
func TestOwnershipLeaseTimingsRenderOnceForBothBinaries(t *testing.T) {
	keys := []string{
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION",
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE",
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD",
	}
	for _, c := range []struct {
		name  string
		extra map[string]any
		want  []string
		read  deploy.OwnershipLeaseTimings
	}{
		{"the chart's default, Diego 2026-11-25", nil, []string{`"30s"`, `"20s"`, `"5s"`},
			deploy.OwnershipLeaseTimings{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}},
		{"a cluster's own", map[string]any{"cdc": map[string]any{"ownershipLease": map[string]any{"duration": "41s", "renewDeadline": "27s", "retryPeriod": "7s"}}},
			[]string{`"41s"`, `"27s"`, `"7s"`},
			deploy.OwnershipLeaseTimings{Duration: 41 * time.Second, RenewDeadline: 27 * time.Second, RetryPeriod: 7 * time.Second}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			for i, key := range keys {
				core, cdc := otelKeyLines(t, c.extra, key)
				want := key + ": " + c.want[i]
				if len(core) != 1 || core[0] != want {
					t.Errorf("core-provider's ConfigMap: %v, want exactly [%s]", core, want)
				}
				if len(cdc) != 1 || cdc[0] != want {
					t.Errorf("the controllers' ConfigMap: %v, want exactly [%s]", cdc, want)
				}
				if len(core) == 1 {
					k, v, _ := strings.Cut(core[0], ": ")
					if uq, err := strconv.Unquote(v); err == nil {
						env[k] = uq
					}
				}
			}
			got, err := deploy.OwnershipLeaseTimingsFromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
			if err != nil || got != c.read {
				t.Errorf("core-provider reads its rendered ConfigMap as %s, %v; want %s", got, err, c.read)
			}
		})
	}
}

// The chart refuses a set the binaries would refuse, naming the three, before anything is rendered from
// it, twice over: values.schema.json (the format, and all three present), and the template's own check
// (core-provider.ownershipLeaseEnv: client-go's order rule, and the format again). The template is also
// run with the schema removed, so each of its arms is seen to refuse on its own. CONTROL: valid sets render.
func TestOwnershipLeaseTimingsChartRefusesAnInvalidSet(t *testing.T) {
	render := func(withSchema bool, d, r, p string) error {
		chrt, err := loadChartForTest(chartDir)
		if err != nil {
			t.Fatal(err)
		}
		if !withSchema {
			chrt.Schema = nil
		}
		extra := map[string]any{"cdc": map[string]any{"ownershipLease": map[string]any{"duration": d, "renewDeadline": r, "retryPeriod": p}}}
		rv, err := chartutil.ToRenderValues(chrt, extra, chartutil.ReleaseOptions{Name: "core-provider", Namespace: "krateo-system", IsInstall: true}, chartutil.DefaultCapabilities)
		if err != nil {
			return err // the schema's refusal
		}
		_, err = engine.Render(chrt, rv)
		return err
	}
	for _, c := range []struct{ d, r, p, says string }{
		{"20s", "20s", "5s", "duration must be greater than renewDeadline"},
		{"20s", "25s", "5s", "duration must be greater than renewDeadline"},
		{"30s", "6s", "5s", "renewDeadline must be greater than retryPeriod x 1.2"},
		{"30s", "20s", "20s", "renewDeadline must be greater than retryPeriod x 1.2"},
		{"30s", "20s", "0s", "greater than zero"},
		{"30", "20s", "5s", "each a Go duration"},
		{"30s", "20s", "", "all three are required"},
	} {
		if err := render(true, c.d, c.r, c.p); err == nil {
			t.Errorf("%s/%s/%s: rendered with the schema", c.d, c.r, c.p)
		}
		err := render(false, c.d, c.r, c.p)
		named := `duration="` + c.d + `" renewDeadline="` + c.r + `" retryPeriod="` + c.p + `"`
		if err == nil || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), named) {
			t.Errorf("%s/%s/%s without the schema: %v; want the template's refusal naming the three (%s) and saying %q", c.d, c.r, c.p, err, named, c.says)
		}
	}
	for _, ok := range [][3]string{{"60s", "40s", "15s"}, {"1m30s", "1m", "20s"}, {"7s", "6100ms", "5s"}, {"15s", "10s", "2s"}} {
		for _, withSchema := range []bool{true, false} {
			if err := render(withSchema, ok[0], ok[1], ok[2]); err != nil {
				t.Errorf("control %v (schema %v): %v", ok, withSchema, err)
			}
		}
	}
}
