package chartfs

import (
	"fmt"
	"io"
	"strings"

	"github.com/gobuffalo/flect"

	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartmeta"
	"github.com/krateo-platformops/core-provider/internal/tools/strutil"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func GroupVersionKind(fs *ChartFS) (schema.GroupVersionKind, error) {
	fin, err := fs.Open(fs.RootDir() + "/Chart.yaml")
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	defer fin.Close()

	din, err := io.ReadAll(fin)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}

	res := map[string]any{}
	if err := yaml.Unmarshal(din, &res); err != nil {
		return schema.GroupVersionKind{}, err
	}

	// Via chartmeta rather than res["version"].(string): the assertion PANICS, and the input that
	// triggers it is an ordinary chart. yaml.v2 resolves YAML 1.1, so `name: y` / `no` / `on` / `off`
	// arrive as bools and `version: 1.0` as a float64 — and all of those are valid Helm chart names.
	// A panic here is a crash loop that takes down every composition this controller serves.
	version, err := chartmeta.StringField(res, "version", fs.RootDir()+"/Chart.yaml")
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	name, err := chartmeta.StringField(res, "name", fs.RootDir()+"/Chart.yaml")
	if err != nil {
		return schema.GroupVersionKind{}, err
	}

	return schema.GroupVersionKind{
		Group:   "composition.krateo.io",
		Version: fmt.Sprintf("v%s", strings.ReplaceAll(version, ".", "-")),
		Kind:    flect.Pascalize(strutil.ToGolangName(name)),
	}, nil
}
