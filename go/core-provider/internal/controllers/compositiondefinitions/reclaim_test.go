package compositiondefinitions

import (
	"context"
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #327. The reclaim retires exactly what findLeakedControllers reports and nothing else, and one
// controller that cannot go does not keep the others. The predicate itself is TestFindLeakedControllers;
// these cases ask what the reclaim DOES with its answer, through a recording retire, so they need no
// templates or cluster.
func TestReclaimLeakedControllers(t *testing.T) {
	type call struct{ version string }
	recorder := func(failOn string) (func(context.Context, schema.GroupVersionResource) error, *[]call) {
		var calls []call
		return func(_ context.Context, gvr schema.GroupVersionResource) error {
			if gvr.Group != leakGroup || gvr.Resource != leakResource {
				t.Errorf("retire got %s, want group %s resource %s", gvr, leakGroup, leakResource)
			}
			calls = append(calls, call{gvr.Version})
			if gvr.Version == failOn {
				return errors.New("drain timed out")
			}
			return nil
		}, &calls
	}
	versions := func(cs []call) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.version)
		}
		return out
	}

	// THE SUBJECT, 057's state: the leaked v0-3-480 controller is retired, the current one is not.
	t.Run("the leaked controller is retired and the current one is not", func(t *testing.T) {
		e := leakExternal(t, leakCRD("v0-3-484", "vacuum"),
			[]*appsv1.Deployment{leakDeploy("installers-v0-3-484-controller"), leakDeploy("installers-v0-3-480-controller")},
			leakComposition("installer", "v0-3-484"))
		retire, calls := recorder("")
		reclaimed, failed, _, err := e.reclaimLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS, retire)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := versions(*calls); !slices.Equal(got, []string{"v0-3-480"}) {
			t.Fatalf("retired %v, want exactly [v0-3-480]", got)
		}
		if !slices.Equal(reclaimed, []string{"installers-v0-3-480-controller"}) || len(failed) != 0 {
			t.Errorf("reclaimed %v failed %v, want [installers-v0-3-480-controller] and none failed", reclaimed, failed)
		}
	})

	// THE REFUSALS RETIRE NOTHING. Each is a reason the predicate gives for keeping everything or a
	// candidate; the reclaim must not reach retire at all.
	refusals := []struct {
		name  string
		build func() *external
	}{
		{"an unaccounted instance blocks every retirement", func() *external {
			return leakExternal(t, leakCRD("v0-3-484", "vacuum"),
				[]*appsv1.Deployment{leakDeploy("installers-v0-3-480-controller")},
				leakComposition("installer", "v0-3-484"), leakComposition("straggler", "v0-3-480"))
		}},
		{"a version still on the CRD, though not served, is not retired", func() *external {
			return leakExternal(t, leakCRD("v0-3-484", "vacuum"),
				[]*appsv1.Deployment{leakDeploy("installers-vacuum-controller")},
				leakComposition("installer", "v0-3-484"))
		}},
		{"no CRD means no notion of which versions exist, so nothing is retired", func() *external {
			return leakExternal(t, nil, []*appsv1.Deployment{leakDeploy("installers-v0-3-480-controller")})
		}},
	}
	for _, r := range refusals {
		t.Run(r.name, func(t *testing.T) {
			retire, calls := recorder("")
			reclaimed, _, _, err := r.build().reclaimLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS, retire)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(*calls) != 0 || len(reclaimed) != 0 {
				t.Fatalf("retired %v (reclaimed %v), want nothing", versions(*calls), reclaimed)
			}
		})
	}

	// ONE FAILURE DOES NOT STOP THE OTHERS, and a failed one is reported as failed, not reclaimed.
	// v0-3-479 sorts first and fails, so stopping at the first failure would leave v0-3-480 behind.
	t.Run("a controller that cannot go does not keep the others", func(t *testing.T) {
		e := leakExternal(t, leakCRD("v0-3-484", "vacuum"),
			[]*appsv1.Deployment{leakDeploy("installers-v0-3-479-controller"), leakDeploy("installers-v0-3-480-controller"),
				leakDeploy("installers-v0-3-484-controller")},
			leakComposition("installer", "v0-3-484"))
		retire, calls := recorder("v0-3-479")
		reclaimed, failed, _, err := e.reclaimLeakedControllers(context.Background(), leakGVK("v0-3-484"), leakGVR(), "installer", leakNS, retire)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := versions(*calls); !slices.Equal(got, []string{"v0-3-479", "v0-3-480"}) {
			t.Fatalf("retire was asked for %v, want both leaked versions", got)
		}
		if !slices.Equal(reclaimed, []string{"installers-v0-3-480-controller"}) {
			t.Errorf("reclaimed %v, want only the one that succeeded", reclaimed)
		}
		if _, ok := failed["installers-v0-3-479-controller"]; !ok || len(failed) != 1 {
			t.Errorf("failed %v, want exactly installers-v0-3-479-controller", failed)
		}
	})
}
