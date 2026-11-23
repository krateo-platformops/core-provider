//go:build envtest

package composition

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The chart: an app ConfigMap carrying .Values.k, and a post-upgrade hook ConfigMap recording the
// revision whose hook created it. A ConfigMap hook, not a Job: envtest runs no Job controller, and
// the hook's object existing is the witness that it ran.
const (
	stuckChartYAML = "apiVersion: v2\nname: stuck\nversion: 0.1.0\n"
	stuckAppCM     = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  k: {{ .Values.k | quote }}\n"
	stuckHookCM    = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hook-ran\n  annotations:\n    helm.sh/hook: post-upgrade\n" +
		"    helm.sh/hook-delete-policy: before-hook-creation\ndata:\n  revision: {{ .Release.Revision | quote }}\n  k: {{ .Values.k | quote }}\n"
)

func serveStuckChart(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"Chart.yaml": stuckChartYAML, "templates/app.yaml": stuckAppCM, "templates/hook.yaml": stuckHookCM} {
		if err := tw.WriteHeader(&tar.Header{Name: "stuck/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/stuck-0.1.0.tgz"
}

// #563 end to end against real helm storage, as a differential. A release is upgraded (rev 2, its
// post-upgrade hook ran), then put into the exact state a controller killed after applying rev 2's
// manifests and before its post-upgrade hook leaves behind: rev 1 still `deployed`, rev 2
// `pending-upgrade` (helm supersedes rev 1 only when the upgrade completes, action/upgrade.go:473),
// and no hook object. The state is written into helm's own storage rather than produced by an
// upgrade held in a goroutine, because a held upgrade is a live writer that later marks rev 2
// failed; a killed controller writes nothing more. The plumbing client reads it back as the control.
//
//   - fixed: the recovery rolls back to rev 1 (rev 3 = rev 1's values); the next Reconcile with the
//     desired values runs a real upgrade (rev 4), and the post-upgrade hook runs.
//   - control, the behaviour before: rolling back to rev 2 itself (rev 3 = rev 2's values); the
//     next Reconcile finds nothing to change, writes no revision, and the hook never runs.
func TestEnvtest_RecoveryRollsBackToTheLastDeployedSoTheHooksRun(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	for _, fixed := range []bool{true, false} {
		name := "fixed: to the last deployed revision"
		if !fixed {
			name = "control: to the pending revision itself (before #563)"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			env := &envtest.Environment{}
			cfg, err := env.Start()
			if err != nil {
				t.Fatalf("start envtest: %v", err)
			}
			t.Cleanup(func() { _ = env.Stop() })
			kube, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			hc, err := helmv3.NewClient(cfg, helmv3.WithNamespace("default"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = hc.Close() })
			url := serveStuckChart(t)
			values := func(k string) *helmconfig.ActionConfig {
				return &helmconfig.ActionConfig{Values: map[string]any{"k": k}}
			}

			if _, err := hc.Install(ctx, "r", url, &helmconfig.InstallConfig{ActionConfig: values("v1"), Namespace: "default"}); err != nil {
				t.Fatalf("install: %v", err)
			}
			if _, err := hc.Upgrade(ctx, "r", url, &helmconfig.UpgradeConfig{ActionConfig: values("v2")}); err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			hookCM := func() (map[string]string, bool) {
				cm, err := kube.CoreV1().ConfigMaps("default").Get(ctx, "hook-ran", metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return nil, false
				}
				if err != nil {
					t.Fatal(err)
				}
				return cm.Data, true
			}
			if d, ok := hookCM(); !ok || d["revision"] != "2" {
				t.Fatalf("control: rev 2's post-upgrade hook did not run (%v %v), so its absence later would prove nothing", d, ok)
			}

			// The kill: rev 1 back to deployed, rev 2 pending-upgrade, the hook's object gone.
			st := storage.Init(driver.NewSecrets(kube.CoreV1().Secrets("default")))
			for v, s := range map[int]release.Status{1: release.StatusDeployed, 2: release.StatusPendingUpgrade} {
				r, err := st.Get("r", v)
				if err != nil {
					t.Fatal(err)
				}
				r.Info.Status = s
				if err := st.Update(r); err != nil {
					t.Fatal(err)
				}
			}
			if err := kube.CoreV1().ConfigMaps("default").Delete(ctx, "hook-ran", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			pending, err := hc.GetRelease(ctx, "r", &helmconfig.GetConfig{})
			if err != nil || pending == nil || pending.Revision != 2 || pending.Status != helmconfig.StatusPendingUpgrade {
				t.Fatalf("control: the crash state did not take: %+v %v", pending, err)
			}

			var rolled *helmconfig.Release
			if fixed {
				dyn := srFakeDyn(srPortal("1", "x"))
				h := &handler{eventRecorder: *event.NewAPIRecorder(events.NewFakeRecorder(10))}
				_, rolled, _, err = h.recoverStuckOperation(ctx, srPortal("1", "x"), hc, pending, "r", reinstallGate{}, srOpts(dyn), logging.NewNopLogger())
			} else {
				var op *helmconfig.OperationResult
				op, err = hc.Rollback(ctx, "r", rollbackConfig(pending.Revision))
				rolled = releaseOf(op)
			}
			if err != nil || rolled == nil {
				t.Fatalf("recovery: %+v %v", rolled, err)
			}
			wantK := map[bool]string{true: "v1", false: "v2"}[fixed]
			if rolled.Revision != 3 || rolled.Status != helmconfig.StatusDeployed || rolled.Values["k"] != wantK {
				t.Fatalf("the rollback wrote revision %d %s with k=%v; want revision 3 deployed with k=%s", rolled.Revision, rolled.Status, rolled.Values["k"], wantK)
			}

			res, err := hc.Reconcile(ctx, "r", url, &helmconfig.UpgradeConfig{ActionConfig: values("v2")})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			d, ran := hookCM()
			t.Logf("after the reconcile: revision %d, changed %v, desiredStateChange %q, hook object %v %v",
				res.Release.Revision, res.Changed, res.DesiredStateChange, ran, d)
			if fixed {
				if !upgradeRanHooks(res, err) || res.Release.Revision != 4 || !ran || d["revision"] != "4" || d["k"] != "v2" {
					t.Fatalf("the reconcile after recovering to the last deployed revision did not run a real upgrade with its hook")
				}
			} else {
				if upgradeRanHooks(res, err) || ran {
					t.Fatalf("control: rolling back to the pending revision itself was expected to leave the hook unrun (the #563 defect), but it ran")
				}
			}
		})
	}
}
