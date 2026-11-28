package chart

import (
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	helmchart "helm.sh/helm/v3/pkg/chart"
	"sigs.k8s.io/yaml"
)

// Info reads the chart's own Chart.yaml, from the same fs and dir the chart's contents are
// rendered from (core-provider#671): the version, and helm's maintainers list. It reads nothing
// else and fetches nothing. A Chart.yaml that cannot be read or parsed, or that names no version,
// returns an error and the caller leaves status.chart as it was: nothing is invented.
//
// A maintainer with an empty name is left out (helm's shape requires a name, and the CRD refuses an
// entry without one, which would fail the whole status write). A chart with no maintainers returns
// the version and a nil list, so the field stays absent rather than an empty list.
func Info(fsys fs.FS, dir string) (*v1alpha1.ChartStatus, error) {
	b, err := fs.ReadFile(fsys, path.Join(dir, "Chart.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading Chart.yaml: %w", err)
	}
	var md helmchart.Metadata
	if err := yaml.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("parsing Chart.yaml: %w", err)
	}
	return chartStatus(&md)
}

func chartStatus(md *helmchart.Metadata) (*v1alpha1.ChartStatus, error) {
	if md.Version == "" {
		return nil, errors.New("Chart.yaml names no version")
	}
	cs := &v1alpha1.ChartStatus{Version: md.Version}
	for _, m := range md.Maintainers {
		if m == nil || m.Name == "" {
			continue
		}
		cs.Maintainers = append(cs.Maintainers, v1alpha1.ChartMaintainer{Name: m.Name, Email: m.Email, URL: m.URL})
	}
	return cs, nil
}
