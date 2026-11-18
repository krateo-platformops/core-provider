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
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// A chart with one app ConfigMap and one post-install hook Job, the OpenStack-Helm shape: the Job
// must outlive its completion (no hook-succeeded policy), so only before-hook-creation is set.
const (
	hookJobsChartYAML = "apiVersion: v2\nname: hookjobs\nversion: 0.1.0\n"
	hookJobsAppCM     = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  k: v\n"
	hookJobsJob       = "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: db-sync\n  annotations:\n    helm.sh/hook: post-install,post-upgrade\n" +
		"    helm.sh/hook-delete-policy: before-hook-creation\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n" +
		"      containers:\n      - name: c\n        image: busybox\n"
)

func serveHookJobsChart(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"Chart.yaml": hookJobsChartYAML, "templates/app.yaml": hookJobsAppCM, "templates/job.yaml": hookJobsJob} {
		if err := tw.WriteHeader(&tar.Header{Name: "hookjobs/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)
	return srv.URL + "/hookjobs-0.1.0.tgz"
}

// completeJobs plays the Job controller envtest does not run: it marks every Job in ns Complete, so
// helm's wait on a hook Job returns, as on a real cluster.
func completeJobs(ctx context.Context, t *testing.T, kube kubernetes.Interface, ns string) {
	go func() {
		for ctx.Err() == nil {
			jobs, err := kube.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
			if err == nil {
				for i := range jobs.Items {
					j := &jobs.Items[i]
					if j.Status.Succeeded > 0 {
						continue
					}
					now := metav1.Now()
					j.Status.StartTime, j.Status.CompletionTime, j.Status.Succeeded = &now, &now, 1
					j.Status.Conditions = []batchv1.JobCondition{
						{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
						{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
					}
					_, _ = kube.BatchV1().Jobs(ns).UpdateStatus(ctx, j, metav1.UpdateOptions{})
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// #576's premises against real helm, and the deletion against a real apiserver: (1) a hook Job is in
// the release's own record before the uninstall, with the manifest the identity is read from;
// (2) helm's uninstall LEAVES it (the problem itself, as the control); (3) the recorded identity
// deletes it.
func TestEnvtest_HookJobsAreRecordedLeftByHelmAndDeletedByIdentity(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	kube := kubernetes.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	completeJobs(ctx, t, kube, "default")

	hc, err := helmv3.NewClient(cfg, helmv3.WithNamespace("default"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hc.Close() })
	url := serveHookJobsChart(t)
	if _, err := hc.Install(ctx, "r", url, &helmconfig.InstallConfig{ActionConfig: &helmconfig.ActionConfig{}, Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	// (1) recorded, read the way Delete reads it: GetRelease before the uninstall.
	rel, err := hc.GetRelease(ctx, "r", &helmconfig.GetConfig{})
	if err != nil || rel == nil {
		t.Fatalf("get release: %v %v", rel, err)
	}
	refs, err := recordedHookJobs(rel)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != (hookJobRef{Namespace: "default", Name: "db-sync"}) {
		t.Fatalf("recorded hook Jobs = %+v, want default/db-sync (hooks in the record: %d)", refs, len(rel.Hooks))
	}

	// (2) helm's uninstall leaves the hook Job behind: the defect, and the control for (3).
	if _, err := hc.Uninstall(ctx, "r", &helmconfig.UninstallConfig{IgnoreNotFound: true, Timeout: time.Minute}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := kube.CoreV1().ConfigMaps("default").Get(ctx, "app", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("control: the release's own ConfigMap should be gone after uninstall, got %v", err)
	}
	if _, err := kube.BatchV1().Jobs("default").Get(ctx, "db-sync", metav1.GetOptions{}); err != nil {
		t.Fatalf("premise: helm should LEAVE the hook Job after uninstall (#576), got %v", err)
	}
	if after, err := hc.GetRelease(ctx, "r", &helmconfig.GetConfig{}); err != nil || after != nil {
		t.Fatalf("premise: the release record is gone after uninstall, so the hooks must be recorded BEFORE it (got %v %v)", after, err)
	}

	// (3) the recorded identity deletes it.
	remaining, err := deleteHookJobs(ctx, dyn, refs)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("deleteHookJobs: remaining %v err %v", remaining, err)
	}
	waitUntil(t, "the hook Job to be deleted", 30*time.Second, func() bool {
		_, err := kube.BatchV1().Jobs("default").Get(ctx, "db-sync", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
}
