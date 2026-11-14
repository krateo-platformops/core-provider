package clusterkube

import (
	"context"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	targetName = "prod-eu"
	// targetNS is the namespace the KubernetesTarget lives in — the same namespace the
	// referencing object resolves its targetRef in (KubernetesTarget is namespaced).
	targetNS   = "demo-system"
	secretName = "prod-eu-kubeconfig"
	secretNS   = "krateo-system"
)

func remoteDeploy() *compositiondefinitionsv1alpha1.DeploymentTarget {
	return &compositiondefinitionsv1alpha1.DeploymentTarget{
		TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: targetName},
	}
}

func kubernetesTarget() *compositiondefinitionsv1alpha1.KubernetesTarget {
	ref := rtv1.SecretKeySelector{Key: "kubeconfig"}
	ref.Name = secretName
	ref.Namespace = secretNS
	return &compositiondefinitionsv1alpha1.KubernetesTarget{
		ObjectMeta: metav1.ObjectMeta{Name: targetName, Namespace: targetNS},
		Spec:       compositiondefinitionsv1alpha1.KubernetesTargetSpec{KubeconfigRef: ref},
	}
}

func kubeconfigSecret(data string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
		Data:       map[string][]byte{"kubeconfig": []byte(data)},
	}
}

func newFakeClient(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = compositiondefinitionsv1alpha1.SchemeBuilder.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func TestIsRemote(t *testing.T) {
	cases := []struct {
		name   string
		target *compositiondefinitionsv1alpha1.DeploymentTarget
		want   bool
	}{
		{"nil deploy", nil, false},
		{"empty deploy", &compositiondefinitionsv1alpha1.DeploymentTarget{}, false},
		{"nil targetRef", &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: nil}, false},
		{"empty target name", &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{}}, false},
		{"named target", remoteDeploy(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRemote(tc.target); got != tc.want {
				t.Fatalf("IsRemote() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemote_NotRemote(t *testing.T) {
	c := newFakeClient().Build()
	if _, err := Remote(context.Background(), c, targetNS, &compositiondefinitionsv1alpha1.DeploymentTarget{}); err == nil {
		t.Fatal("expected error for a deploy with no targetRef")
	}
}

func TestRemote_TargetNotFound(t *testing.T) {
	c := newFakeClient(kubeconfigSecret("x")).Build() // secret exists, target does not
	if _, err := Remote(context.Background(), c, targetNS, remoteDeploy()); err == nil {
		t.Fatal("expected error when the KubernetesTarget is missing")
	}
}

func TestRemote_SecretNotFound(t *testing.T) {
	c := newFakeClient(kubernetesTarget()).Build() // target exists, secret does not
	if _, err := Remote(context.Background(), c, targetNS, remoteDeploy()); err == nil {
		t.Fatal("expected error when the kubeconfig secret is missing")
	}
}

func TestRemote_EmptyKubeconfig(t *testing.T) {
	c := newFakeClient(kubernetesTarget(), kubeconfigSecret("")).Build()
	if _, err := Remote(context.Background(), c, targetNS, remoteDeploy()); err == nil {
		t.Fatal("expected error for empty kubeconfig")
	}
}

func TestRemote_InvalidKubeconfig(t *testing.T) {
	c := newFakeClient(kubernetesTarget(), kubeconfigSecret("not-a-valid-kubeconfig")).Build()
	if _, err := Remote(context.Background(), c, targetNS, remoteDeploy()); err == nil {
		t.Fatal("expected error for invalid kubeconfig")
	}
}

func TestRemote_ValidKubeconfig(t *testing.T) {
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: target
  cluster:
    server: https://target.example.com:6443
    insecure-skip-tls-verify: true
contexts:
- name: target
  context:
    cluster: target
    user: target
current-context: target
users:
- name: target
  user:
    token: abc123
`
	c := newFakeClient(kubernetesTarget(), kubeconfigSecret(kubeconfig)).Build()
	got, err := Remote(context.Background(), c, targetNS, remoteDeploy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Config.Host != "https://target.example.com:6443" {
		t.Fatalf("unexpected host: %q", got.Config.Host)
	}
	if got.Kube == nil || got.Dynamic == nil || got.Clientset == nil {
		t.Fatal("expected non-nil clients")
	}
	if got.SecretResourceVersion == "" {
		t.Fatal("expected SecretResourceVersion to be captured from the kubeconfig Secret")
	}
	if got.AuthnURL != "" {
		t.Fatalf("a target without spec.authnUrl must report none, got %q", got.AuthnURL)
	}

	// #251 item 4: the target's own authn URL is carried to the caller, which gives it to a
	// controller running there.
	kt := kubernetesTarget()
	kt.Spec.AuthnURL = "http://authn.auth-system.svc.cluster.local:9443"
	c = newFakeClient(kt, kubeconfigSecret(kubeconfig)).Build()
	got, err = Remote(context.Background(), c, targetNS, remoteDeploy())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AuthnURL != kt.Spec.AuthnURL {
		t.Fatalf("AuthnURL = %q, want the KubernetesTarget's %q", got.AuthnURL, kt.Spec.AuthnURL)
	}
}
