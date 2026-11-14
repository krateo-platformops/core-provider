// Package clusterkube resolves the Kubernetes clients used to deploy the
// composition-dynamic-controller (and its generated CRD and RBAC) to the cluster
// selected by a CompositionDefinition: the local management cluster (default) or a
// remote target cluster addressed by a kubeconfig Secret.
//
// Credentials are consumed as native Kubernetes Secrets and re-read on every
// reconcile, so rotation performed by an external secret manager (e.g. External
// Secrets Operator) is picked up automatically.
package clusterkube

import (
	"context"
	"fmt"
	"strings"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Clients bundles the clients needed to install/observe resources in a target cluster.
type Clients struct {
	Kube      client.Client
	Dynamic   dynamic.Interface
	Clientset kubernetes.Interface
	Config    *rest.Config
	// SecretResourceVersion is the resourceVersion of the kubeconfig Secret these
	// clients were built from, for rotation traceability (empty for local targets).
	SecretResourceVersion string
	// AuthnURL is the KubernetesTarget's spec.authnUrl: the authn a controller running in that
	// target calls (#251 item 4). Empty when unset, and for local targets.
	AuthnURL string
}

// IsRemote reports whether the deployment target references a remote cluster.
func IsRemote(target *compositiondefinitionsv1alpha1.DeploymentTarget) bool {
	return target != nil && target.TargetRef != nil && target.TargetRef.Name != ""
}

// Remote builds the clients for the remote cluster referenced by target.TargetRef. It
// resolves the namespaced KubernetesTarget in namespace (the referencing
// CompositionDefinition's OWN namespace), then the kubeconfig Secret it points at; both are
// read from the management cluster via mgmt.
func Remote(ctx context.Context, mgmt client.Client, namespace string, target *compositiondefinitionsv1alpha1.DeploymentTarget) (*Clients, error) {
	if !IsRemote(target) {
		return nil, fmt.Errorf("deployment target does not reference a KubernetesTarget")
	}

	kt := &compositiondefinitionsv1alpha1.KubernetesTarget{}
	if err := mgmt.Get(ctx, types.NamespacedName{Namespace: namespace, Name: target.TargetRef.Name}, kt); err != nil {
		return nil, fmt.Errorf("reading KubernetesTarget %s/%s: %w", namespace, target.TargetRef.Name, err)
	}
	ref := kt.Spec.KubeconfigRef

	secret := &corev1.Secret{}
	if err := mgmt.Get(ctx, types.NamespacedName{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}, secret); err != nil {
		return nil, fmt.Errorf("reading kubeconfig secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	rc, err := restConfigFromSecret(secret, ref.Key)
	if err != nil {
		return nil, fmt.Errorf("building rest config from secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	clients, err := clientsFor(rc)
	if err != nil {
		return nil, err
	}
	clients.SecretResourceVersion = secret.ResourceVersion
	clients.AuthnURL = kt.Spec.AuthnURL
	return clients, nil
}

// restConfigFromSecret builds a *rest.Config for a target from one of the two accepted Secret shapes,
// so core-provider stays a pure Secret consumer (no TokenRequest/rotation loop of its own — those are
// delegated to ESO):
//
//	(a) a full kubeconfig blob under the referenced key (static kubeconfig, or one carrying an
//	    exec-plugin credential for cloud identity — GKE/EKS/AKS — which client-go resolves), or
//	(b) token + server (+ optional ca.crt) keys — the shape ESO emits when it mints and rotates a
//	    target ServiceAccount token via the TokenRequest API.
//
// Sane QPS/Burst are set so remote reconciles are not throttled by client-go defaults.
func restConfigFromSecret(secret *corev1.Secret, key string) (*rest.Config, error) {
	var rc *rest.Config

	if kc := secret.Data[key]; len(kc) > 0 {
		// Form (a): a full kubeconfig under the referenced key.
		parsed, err := clientcmd.RESTConfigFromKubeConfig(kc)
		if err != nil {
			return nil, fmt.Errorf("parsing kubeconfig under key %q: %w", key, err)
		}
		rc = parsed
	} else if token, server := secret.Data["token"], secret.Data["server"]; len(token) > 0 && len(server) > 0 {
		// Form (b): token + server (+ ca.crt) — an ESO-minted/rotated SA token.
		rc = &rest.Config{
			Host:        strings.TrimSpace(string(server)),
			BearerToken: strings.TrimSpace(string(token)),
		}
		if ca := secret.Data["ca.crt"]; len(ca) > 0 {
			rc.TLSClientConfig = rest.TLSClientConfig{CAData: ca}
		}
	} else {
		return nil, fmt.Errorf("secret carries neither a kubeconfig under key %q nor token+server keys", key)
	}

	if rc.QPS == 0 {
		rc.QPS = 20
	}
	if rc.Burst == 0 {
		rc.Burst = 30
	}
	return rc, nil
}

func clientsFor(rc *rest.Config) (*Clients, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	cl, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building target kube client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("building target dynamic client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("building target clientset: %w", err)
	}

	return &Clients{Kube: cl, Dynamic: dyn, Clientset: cs, Config: rc}, nil
}
