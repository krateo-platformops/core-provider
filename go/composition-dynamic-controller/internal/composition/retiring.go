package composition

import (
	"fmt"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
)

// retiringController reports whether this controller is retiring for the composition pkg describes:
// the chart it renders (pkg.Version, the chart recorded for this controller's own version) is not
// the CompositionDefinition's current one (pkg.DefinitionVersion). Such a controller must not move
// the release forward (core-provider#519).
//
// During a version switch the old version's controller still receives the composition until its
// version label moves, and it rendered its OLD chart with the NEW values: helm upgraded nova
// 0.3.62 with 0.3.64's values (openstack-as-a-service, a Keystone lockout), and on krateo-057 the
// draining installers controller started an upgrade it never finished (core-provider#520). A
// retiring controller still heals drift against the deployed release, and leaves every change,
// install and recovery to the current version's controller.
//
// Unknown is not retiring: with either version missing (a definition without a chart version, or
// a lookup that does not resolve one) the controller behaves as before.
func retiringController(pkg *archive.Info) (bool, string) {
	if pkg == nil || pkg.Version == "" || pkg.DefinitionVersion == "" || pkg.Version == pkg.DefinitionVersion {
		return false, ""
	}
	return true, fmt.Sprintf("this controller renders chart %s; the CompositionDefinition's current chart is %s", pkg.Version, pkg.DefinitionVersion)
}
