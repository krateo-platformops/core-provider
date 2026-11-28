package composition

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/plumbing/helm/getter"
	"github.com/krateo-platformops/plumbing/helm/getter/cache"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// The two inputs of chart-inspector's answer that the derived-RBAC cache (#697) takes from outside the
// pass: whether the chart reads cluster state with `lookup`, and which chart-inspector build answers.

// ---- chart-inspector's version -------------------------------------------------------------------

// inspectorVersions knows the build version chart-inspector last reported. Every render response
// reports it (chartinspector.WithVersionObserver), and current() asks /healthz when none has been
// seen for `every`, so an upgrade is noticed without a render. "" is the version of an inspector that
// sends no header, which is how an older one keys.
type inspectorVersions struct {
	mu      sync.Mutex
	version string
	probed  time.Time
	every   time.Duration
}

// current returns the last known version, asking chart-inspector first when the last probe is older
// than every. A failed probe keeps the last known version: an unreachable inspector is not an upgraded
// one, and the render that does reach it reports the truth. A nil tracker reads "".
func (v *inspectorVersions) current(ctx context.Context, server string) string {
	if v == nil {
		return ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.probed.IsZero() || time.Since(v.probed) >= v.every {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ci := chartinspector.NewChartInspector(server)
		if got, err := ci.Version(pctx); err == nil {
			v.version = got
		}
		cancel()
		v.probed = time.Now()
	}
	return v.version
}

// observe records the version a render response carried.
func (v *inspectorVersions) observe(version string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.version = version
	v.mu.Unlock()
}

// ---- `lookup` in the chart -----------------------------------------------------------------------

// lookupWord matches the template function by word boundary. A comment, a string, a value named
// "lookup" all match: a false positive only disables the cache for that chart, the safe direction.
// A false negative would apply stale RBAC, so this errs wide.
var lookupWord = regexp.MustCompile(`\blookup\b`)

// chartIdent is a chart's identity as the cache sees it: where it comes from and which version.
// (Credentials do not change what the archive is, so they are not part of it.)
type chartIdent struct{ URL, Repo, Version string }

// chartLookupScanner says whether a chart calls `lookup`, once per chart identity. The archive comes
// from the same disk cache the helm client reads (plumbing's default directory), which is the chart
// helm's Reconcile loads, so a chart already fetched is not fetched again; the first pass that needs
// a chart not yet cached fetches it once, and the Reconcile that follows finds it there.
//
// The identity has no archive digest: reading the archive on every pass to digest it is the cost this
// avoids. A chart republished under the same version is as stale to helm's own cache.
type chartLookupScanner struct {
	mu   sync.Mutex
	memo map[chartIdent]bool
	// scan decides one chart. Replaced in tests.
	scan func(ctx context.Context, id chartIdent, auth *archive.Auth, insecureTLS bool) (bool, error)

	once sync.Once
	disk *cache.DiskCache
	derr error
}

func newChartLookupScanner() *chartLookupScanner {
	s := &chartLookupScanner{memo: map[chartIdent]bool{}}
	s.scan = s.scanArchive
	return s
}

// usesLookup reports whether any chart this composition's RBAC is derived from calls `lookup`. The
// cdc renders the chart recorded for its own version (#461) and chart-inspector the definition's
// current one, so both are scanned when they differ. An error means unknown, which the caller must
// not read as "no".
func (s *chartLookupScanner) usesLookup(ctx context.Context, pkg *archive.Info) (bool, error) {
	if s == nil || s.scan == nil {
		return false, fmt.Errorf("no chart scanner")
	}
	ids := []chartIdent{{pkg.URL, pkg.Repo, pkg.Version}}
	if pkg.DefinitionVersion != "" && pkg.DefinitionVersion != pkg.Version {
		ids = append(ids, chartIdent{pkg.URL, pkg.Repo, pkg.DefinitionVersion})
	}
	found := false
	for _, id := range ids {
		s.mu.Lock()
		uses, known := s.memo[id]
		s.mu.Unlock()
		if !known {
			var err error
			uses, err = s.scan(ctx, id, pkg.Auth, pkg.InsecureSkipTLSverify)
			if err != nil {
				return false, fmt.Errorf("scanning chart %s@%s: %w", id.URL, id.Version, err)
			}
			s.mu.Lock()
			s.memo[id] = uses
			s.mu.Unlock()
		}
		found = found || uses
	}
	return found, nil
}

func (s *chartLookupScanner) scanArchive(ctx context.Context, id chartIdent, auth *archive.Auth, insecureTLS bool) (bool, error) {
	s.once.Do(func() { s.disk, s.derr = cache.NewDiskCache() })
	if s.derr != nil {
		return false, s.derr
	}
	opts := []getter.Option{
		getter.WithVersion(id.Version),
		getter.WithRepo(id.Repo),
		getter.WithCache(s.disk),
		getter.WithInsecureSkipVerifyTLS(insecureTLS),
	}
	if auth != nil {
		opts = append(opts, getter.WithCredentials(auth.Username, auth.Password))
	}
	r, _, err := getter.Get(ctx, id.URL, opts...)
	if err != nil {
		return false, err
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}
	return archiveUsesLookup(r)
}

// archiveUsesLookup reports whether any template of the chart in r, or of any subchart at any depth,
// mentions `lookup`. Every file under templates/ counts (helpers, partials, *.tpl, NOTES), and so does
// each chart's values.yaml: a template that runs `tpl` over a value renders whatever that value holds.
func archiveUsesLookup(r io.Reader) (bool, error) {
	ch, err := loader.LoadArchive(r)
	if err != nil {
		return false, fmt.Errorf("loading chart archive: %w", err)
	}
	return chartUsesLookup(ch), nil
}

func chartUsesLookup(ch *chart.Chart) bool {
	for _, f := range ch.Templates {
		if lookupWord.Match(f.Data) {
			return true
		}
	}
	for _, f := range ch.Raw {
		if f.Name == chartValuesFile && lookupWord.Match(f.Data) {
			return true
		}
	}
	for _, dep := range ch.Dependencies() {
		if chartUsesLookup(dep) {
			return true
		}
	}
	return false
}

const chartValuesFile = "values.yaml"
