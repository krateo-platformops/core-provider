package deploy

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// #313. Every Undeploy deletes the controller's ServiceAccount, so the session credential authn
// stored for that identity is orphaned unless Undeploy deletes it too: authn never does. The name is
// asserted as a LITERAL, not recomputed through endpoints.SecretName: a test that rebuilt it the same
// way the code does could not fail on a wrong name.
func TestUndeploy_DeletesTheIdentitysClientConfigSecret(t *testing.T) {
	const (
		authnNS = "krateo-system" // AuthnNamespace left empty: the default
		mine    = "cdc-krateo-system-fireworksapps-v1-0-0-clientconfig"
		other   = "cdc-krateo-system-someotherkind-v1-0-0-clientconfig"
	)
	kube := clientfake.NewClientBuilder().WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: authnNS, Name: mine}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: authnNS, Name: other}},
	).Build()

	dyn := relocatingCluster()
	if err := Undeploy(context.Background(), nil, UndeployOptions{
		KubeClient: kube, DynamicClient: dyn, GVR: guardGVR,
		Namespace: keepSharedOldNS, ControllerNamespace: keepSharedOldNS, SkipCRD: true, Relocating: true,
		RBACFolderPath: "testdata", DeploymentTemplatePath: "testdata/deploy.yaml",
		ConfigmapTemplatePath: "testdata/cm.yaml", JsonSchemaTemplatePath: "testdata/configmap_jsonschema.yaml",
		ServiceTemplatePath: "testdata/service.yaml",
	}); err != nil {
		t.Fatal(err)
	}

	get := func(name string) error {
		return kube.Get(context.Background(), types.NamespacedName{Namespace: authnNS, Name: name}, &corev1.Secret{})
	}
	if err := get(mine); !apierrors.IsNotFound(err) {
		t.Errorf("the removed identity's clientconfig Secret %s survived (get err = %v)", mine, err)
	}
	if err := get(other); err != nil {
		t.Errorf("another identity's clientconfig Secret %s was touched: %v", other, err)
	}
}
