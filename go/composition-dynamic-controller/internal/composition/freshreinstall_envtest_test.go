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
	"strings"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The chart: an app ConfigMap; a post-install hook JOB whose hook-delete-policy is set WITHOUT
// before-hook-creation (hook-failed only), so a leftover completed one collides with the reinstall's,
// which is the case the recorded-Job deletion exists for. NOT "no policy": helm 3 applies
// before-hook-creation by default when the annotation is absent (measured here first: with no policy
// the reinstall over a leftover Job succeeded). And a pre-delete hook ConfigMap whose existence is the
// witness that a pre-delete hook ran.
const (
	frChartYAML  = "apiVersion: v2\nname: fresh\nversion: 0.1.0\n"
	frAppCM      = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  k: \"v\"\n"
	frHookJob    = "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: hook-job\n  annotations:\n    helm.sh/hook: post-install\n    helm.sh/hook-delete-policy: hook-failed\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n      - name: c\n        image: busybox\n"
	frPreDelete  = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: predelete-ran\n  annotations:\n    helm.sh/hook: pre-delete\ndata:\n  ran: \"yes\"\n"
	frReleaseNS  = "default"
	frReleaseKey = "r"
)

func serveFreshChart(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"Chart.yaml": frChartYAML, "templates/app.yaml": frAppCM,
		"templates/hookjob.yaml": frHookJob, "templates/predelete.yaml": frPreDelete} {
		if err := tw.WriteHeader(&tar.Header{Name: "fresh/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
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
	return srv.URL + "/fresh-0.1.0.tgz"
}

// core-provider#629 end to end against real helm storage. A first install runs (its post-install hook
// Job created and completed), then the release is put into the state a controller killed mid-install
// leaves: revision 1 pending-install, no deployed revision, the hook Job still there.
//
//   - fixed: the recovery uninstalls WITHOUT hooks (the pre-delete witness never appears), deletes the
//     recorded hook Job, and reports the release absent; the next install (the runtime's Create) runs
//     and creates exactly one hook Job, a new object.
//   - control A: an uninstall WITH hooks makes the pre-delete witness appear, so its absence above
//     is evidence, not a witness that cannot fire.
//   - control B: without the recorded-Job deletion, the reinstall of a chart whose hook-delete-policy
//     omits before-hook-creation fails on the leftover Job, so the deletion is load-bearing.
func TestEnvtest_InterruptedFreshInstallIsReinstalledWithItsHooks(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	for _, mode := range []string{"fixed", "control A: uninstall with hooks", "control B: no hook-Job deletion"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
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
			realDyn, err := dynamic.NewForConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			hc, err := helmv3.NewClient(cfg, helmv3.WithNamespace(frReleaseNS))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = hc.Close() })
			completeJobs(ctx, t, kube, frReleaseNS)
			url := serveFreshChart(t)
			install := func() error {
				_, err := hc.Install(ctx, frReleaseKey, url, &helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{Timeout: 60 * time.Second}, Namespace: frReleaseNS})
				return err
			}

			if err := install(); err != nil {
				t.Fatalf("first install: %v", err)
			}
			first, err := kube.BatchV1().Jobs(frReleaseNS).Get(ctx, "hook-job", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("control: the first install's post-install hook Job does not exist: %v", err)
			}

			// The kill: revision 1 back to pending-install, as a controller that died mid-install leaves it.
			st := storage.Init(driver.NewSecrets(kube.CoreV1().Secrets(frReleaseNS)))
			r, err := st.Get(frReleaseKey, 1)
			if err != nil {
				t.Fatal(err)
			}
			r.Info.Status = release.StatusPendingInstall
			if err := st.Update(r); err != nil {
				t.Fatal(err)
			}
			pending, err := hc.GetRelease(ctx, frReleaseKey, &helmconfig.GetConfig{})
			if err != nil || pending == nil || pending.Revision != 1 || pending.Status != helmconfig.StatusPendingInstall {
				t.Fatalf("control: the crash state did not take: %+v %v", pending, err)
			}
			if len(pending.Hooks) == 0 {
				t.Fatalf("control: the release record names no hooks, so the recorded-Job deletion would be vacuous")
			}

			predeleteRan := func() bool {
				_, err := kube.CoreV1().ConfigMaps(frReleaseNS).Get(ctx, "predelete-ran", metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return false
				}
				if err != nil {
					t.Fatal(err)
				}
				return true
			}

			switch mode {
			case "fixed":
				compDyn := frFakeDyn(srPortal("1", "x"))
				h := &handler{eventRecorder: *event.NewAPIRecorder(events.NewFakeRecorder(10))}
				gate := reinstallGate{policyAllows: true, cdAnnotationsKnown: true, cdAnnotations: map[string]string{}, dyn: realDyn}
				_, _, uninstalled, err := h.recoverStuckOperation(ctx, srPortal("1", "x"), hc, pending, frReleaseKey, gate, srOpts(compDyn), logging.NewNopLogger())
				if err != nil || !uninstalled {
					t.Fatalf("recovery: uninstalled=%v err=%v", uninstalled, err)
				}
				if predeleteRan() {
					t.Fatal("the recovery's uninstall ran a pre-delete hook: it must run WITHOUT hooks")
				}
				if _, err := kube.BatchV1().Jobs(frReleaseNS).Get(ctx, "hook-job", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatalf("the interrupted install's recorded hook Job was not deleted: %v", err)
				}
				if rel, _ := hc.GetRelease(ctx, frReleaseKey, &helmconfig.GetConfig{}); rel != nil {
					t.Fatalf("the release still exists after the recovery: %+v", rel)
				}
				// The runtime's Create installs it again.
				if err := waitJobGone(ctx, kube, first.UID); err != nil {
					t.Fatal(err)
				}
				if err := install(); err != nil {
					t.Fatalf("the reinstall failed: %v", err)
				}
				jobs, err := kube.BatchV1().Jobs(frReleaseNS).List(ctx, metav1.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				var hookJobs []batchv1.Job
				for _, j := range jobs.Items {
					if j.Name == "hook-job" {
						hookJobs = append(hookJobs, j)
					}
				}
				if len(hookJobs) != 1 || hookJobs[0].UID == first.UID {
					t.Fatalf("after the reinstall: %d hook-job(s), first UID %s; want exactly one, a new object", len(hookJobs), first.UID)
				}
			case "control A: uninstall with hooks":
				if _, err := hc.Uninstall(ctx, frReleaseKey, &helmconfig.UninstallConfig{IgnoreNotFound: true, Timeout: 60 * time.Second}); err != nil {
					t.Fatalf("uninstall with hooks: %v", err)
				}
				if !predeleteRan() {
					t.Fatal("control: an uninstall WITH hooks did not create the pre-delete witness, so its absence proves nothing")
				}
			case "control B: no hook-Job deletion":
				if _, err := hc.Uninstall(ctx, frReleaseKey, &helmconfig.UninstallConfig{DisableHooks: true, IgnoreNotFound: true, Timeout: 60 * time.Second}); err != nil {
					t.Fatalf("uninstall without hooks: %v", err)
				}
				err := install()
				if err == nil || !strings.Contains(err.Error(), "exists") {
					t.Fatalf("control: the reinstall over a leftover hook Job (policy without before-hook-creation) did not fail on it (err=%v), so the deletion is not shown to matter", err)
				}
			}
		})
	}
}

// waitJobGone waits for the background-propagated delete of the first attempt's hook Job to finish.
func waitJobGone(ctx context.Context, kube kubernetes.Interface, uid types.UID) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := kube.BatchV1().Jobs(frReleaseNS).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		found := false
		for _, j := range jobs.Items {
			if j.UID == uid {
				found = true
			}
		}
		if !found {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return context.DeadlineExceeded
}
