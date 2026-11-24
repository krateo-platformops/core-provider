package templates

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

type Renderoptions struct {
	Group     string
	Version   string
	Resource  string
	Namespace string
	Name      string
}

func Values(opts Renderoptions) map[string]any {
	if len(opts.Name) == 0 {
		opts.Name = fmt.Sprintf("%s-controller", opts.Resource)
	}

	if len(opts.Namespace) == 0 {
		opts.Namespace = "default"
	}

	values := map[string]any{
		"apiGroup":        opts.Group,
		"apiVersion":      opts.Version,
		"resource":        opts.Resource,
		"name":            opts.Name,
		"namespace":       opts.Namespace,
		"controllerLease": ControllerLeaseName(opts.Group, opts.Version, opts.Resource),
	}

	return values
}

// maxLeaseName is a Lease name's limit: a DNS subdomain.
const maxLeaseName = 253

// ControllerLeaseName is the coordination.k8s.io Lease a composition controller must hold to
// reconcile its Kind at one version (docs/design/cdc-pooling.md, step 2): <version>.<plural>.<group>.
//
// It names the Kind x version, never the Deployment or pod, so a dedicated controller and a pooled
// one for the same Kind x version compete for the SAME Lease, which is the whole guarantee. It is
// derived here, once, and reaches the controller (COMPOSITION_CONTROLLER_LEASE_NAME, assets/cdc/
// configmap.yaml) and its Role (assets/cdc/rbac/lease-role.yaml) through the same template value,
// so the two cannot disagree.
//
// A name over 253 characters (possible only for a very long group) keeps the version and replaces
// the rest with a hash of plural.group, which stays unique per Kind x version.
func ControllerLeaseName(group, version, resource string) string {
	name := version + "." + resource + "." + group
	if len(name) <= maxLeaseName {
		return name
	}
	sum := sha256.Sum256([]byte(resource + "." + group))
	return version + "." + hex.EncodeToString(sum[:])
}

type Template string

// Render refuses a template that uses a key the caller did not pass (#651). text/template's default
// prints "<no value>" for one, or, through a function such as quote, nothing at all: a Lease named
// "<no value>", an authn URL of "". A key that does not apply is passed as "", never left out.
func (t Template) Render(values map[string]any) ([]byte, error) {
	tpl, err := template.New("template").Funcs(sprig.FuncMap()).Option("missingkey=error").Parse(string(t))
	if err != nil {
		return nil, err
	}

	buf := bytes.Buffer{}
	if err := tpl.Execute(&buf, values); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
