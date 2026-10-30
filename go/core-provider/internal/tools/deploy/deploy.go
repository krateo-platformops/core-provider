package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/plumbing/labels"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/core-provider/internal/tools/kube/watcher"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	definitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	crd "github.com/krateo-platformops/core-provider/internal/tools/crd"
	deployment "github.com/krateo-platformops/core-provider/internal/tools/deployment"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Composition label keys re-exported from plumbing/labels — the single shared source of truth with
	// composition-dynamic-controller (compile-time identical, no cross-repo drift). CompositionVersionLabel
	// is the served version an instance was LAST WRITTEN THROUGH — re-stamped on every update, so it is
	// not an origin version (see policy.versionLabel for what it does and does not mean);
	// CompositionDefinitionName/NamespaceLabel
	// identify the OWNING definition, scoping per-version migration so a version bump of one definition
	// never re-stamps another definition's instances on a shared CRD/Kind.
	CompositionVersionLabel             = labels.CompositionVersionLabel
	CompositionDefinitionNameLabel      = labels.CompositionDefinitionNameLabel
	CompositionDefinitionNamespaceLabel = labels.CompositionDefinitionNamespaceLabel
	CompositionStillExistErr            = "compositions still exist"
	controllerResourceSuffix            = "-controller"
	configmapResourceSuffix             = "-configmap"
)

var (
	ErrCompositionStillExist = errors.New(CompositionStillExistErr)
)

type UndeployOptions struct {
	DiscoveryClient        discovery.CachedDiscoveryInterface
	KubeClient             client.Client
	DynamicClient          dynamic.Interface
	Namespace              string
	GVR                    schema.GroupVersionResource
	Spec                   *definitionsv1alpha1.ChartInfo
	RBACFolderPath         string
	SkipCRD                bool
	DeploymentTemplatePath string
	ServiceTemplatePath    string
	ConfigmapTemplatePath  string
	JsonSchemaTemplatePath string
	JsonSchemaBytes        []byte
	// ApiRefName mirrors DeployOptions: when set, the authn ServiceAccount mapping created on
	// deploy is removed on undeploy. AuthnNamespace is the authn operator namespace it lives in.
	ApiRefName     string
	AuthnNamespace string
	// DrainTimeout bounds how long Undeploy waits for the retiring controller's pod to disappear
	// before it deletes that pod's ServiceAccount and RBAC (#263). Zero means derive it from the
	// rendered Deployment's own terminationGracePeriodSeconds plus a margin, which is what
	// production uses — the kubelet SIGKILLs at the grace period, so the pod cannot outlive it.
	//
	// Set it explicitly only to override that derivation: a cluster whose apiserver is slow to
	// reflect pod deletion may need longer, and tests use a short value so the suite does not pay
	// the full grace period per case.
	DrainTimeout time.Duration

	// KeepShared leaves in place every object another CompositionDefinition's controller for the
	// same Kind+version also uses: the cluster-scoped ClusterRole/ClusterRoleBinding and apiRef RBAC
	// (named {resource}-{apiVersion}, with no namespace in the name), and the credentials
	// Role/RoleBinding when they live outside this definition's namespace. Only the objects in
	// Namespace are removed. Set it whenever the version is still referenced by another definition:
	// deleting the shared objects would strip the surviving controller of its permissions.
	KeepShared bool
}

// (UndeployOptions needs no SnowplowURL: the apiRef ClusterRole + binding are deleted by name.)

type DeployOptions struct {
	GVR                    schema.GroupVersionResource
	DiscoveryClient        discovery.CachedDiscoveryInterface
	KubeClient             client.Client
	DynClient              dynamic.Interface
	Namespace              string
	Spec                   *definitionsv1alpha1.ChartInfo
	RBACFolderPath         string
	DeploymentTemplatePath string
	ConfigmapTemplatePath  string
	JsonSchemaTemplatePath string
	ServiceTemplatePath    string
	JsonSchemaBytes        []byte
	// StatusDataTemplate is the JSON-serialized statusDataTemplate (snowplow
	// widgetDataTemplate shape) from the CompositionDefinition, rendered into the CDC
	// ConfigMap as COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE. Empty disables projection.
	StatusDataTemplate string
	// ApiRefName / ApiRefNamespace identify the RESTAction the CDC resolves (via snowplow)
	// each reconcile to populate the ".api" projection source. Empty disables apiRef
	// resolution. ApiRefExtras is the JSON-serialized inline extras (snowplow
	// spec.apiRef.extras), rendered alongside as COMPOSITION_CONTROLLER_API_REF_*.
	ApiRefName      string
	ApiRefNamespace string
	ApiRefExtras    string
	// AuthnNamespace is the authn operator namespace where the ServiceAccount allowlist
	// mapping is created (when ApiRefName is set). Empty defaults to "krateo-system".
	AuthnNamespace string
	// SnowplowURL is snowplow's base URL. When ApiRefName is set, core-provider calls its
	// dispatch-free GET /rbac to enumerate the RESTAction's read-set and grant it to the
	// per-composition group. Required when ApiRefName is set.
	SnowplowURL string
	// AuthnURL is the authn service base URL. snowplow's /rbac is gated by the same JWT middleware
	// as /call, so core-provider exchanges its projected SA token for an authn-issued service JWT
	// and presents it as the Bearer. Empty ⇒ the /rbac call is unauthenticated (snowplow 401).
	AuthnURL string
	// SelfSAName / SelfSANamespace / SelfGroup identify core-provider's OWN ServiceAccount for its
	// apiRefRBAC authn allowlist mapping — provisioned lazily (ensureSelfAuthnMapping) the first
	// time a composition declares an apiRef, so core-provider can authenticate to snowplow /rbac.
	// Set from CORE_PROVIDER_APIREF_SELF_SA_NAME / _NAMESPACE and CORE_PROVIDER_APIREF_GROUP. Empty
	// SelfSAName disables self-provisioning (apiRefRBAC off).
	SelfSAName      string
	SelfSANamespace string
	SelfGroup       string
	// DryRunServer is used to determine if the deployment should be applied in dry-run mode. This is ignored in lookup mode
	DryRunServer bool
	// Controller carries this CompositionDefinition's per-Kind CDC tuning (spec.controller). Nil, or
	// nil fields within, are omitted from the Deployment template render so the template's
	// chart-global defaults apply — existing definitions render byte-identically.
	Controller *definitionsv1alpha1.ControllerConfig
}

func resourceNamer(resourceName string, chartVersion string) string {
	return fmt.Sprintf("%s-%s", resourceName, chartVersion)
}

func createRBACResources(gvr schema.GroupVersionResource, rbacNSName types.NamespacedName, rbacFolderPath string) (corev1.ServiceAccount, rbacv1.ClusterRole, rbacv1.ClusterRoleBinding, rbacv1.Role, rbacv1.RoleBinding, error) {
	sa := corev1.ServiceAccount{}
	err := objects.CreateK8sObject(&sa, gvr, rbacNSName, filepath.Join(rbacFolderPath, "serviceaccount.yaml"))
	if err != nil {
		return corev1.ServiceAccount{}, rbacv1.ClusterRole{}, rbacv1.ClusterRoleBinding{}, rbacv1.Role{}, rbacv1.RoleBinding{}, err
	}

	clusterrole := rbacv1.ClusterRole{}
	err = objects.CreateK8sObject(&clusterrole, gvr, rbacNSName, filepath.Join(rbacFolderPath, "clusterrole.yaml"))
	if err != nil {
		return corev1.ServiceAccount{}, rbacv1.ClusterRole{}, rbacv1.ClusterRoleBinding{}, rbacv1.Role{}, rbacv1.RoleBinding{}, err
	}

	clusterrolebinding := rbacv1.ClusterRoleBinding{}
	err = objects.CreateK8sObject(&clusterrolebinding, gvr, rbacNSName, filepath.Join(rbacFolderPath, "clusterrolebinding.yaml"), "serviceAccount", sa.Name, "saNamespace", sa.Namespace)
	if err != nil {
		return corev1.ServiceAccount{}, rbacv1.ClusterRole{}, rbacv1.ClusterRoleBinding{}, rbacv1.Role{}, rbacv1.RoleBinding{}, err
	}

	role := rbacv1.Role{}
	err = objects.CreateK8sObject(&role, gvr, rbacNSName, filepath.Join(rbacFolderPath, "compositiondefinition-role.yaml"))
	if err != nil {
		return corev1.ServiceAccount{}, rbacv1.ClusterRole{}, rbacv1.ClusterRoleBinding{}, rbacv1.Role{}, rbacv1.RoleBinding{}, err
	}

	rolebinding := rbacv1.RoleBinding{}
	err = objects.CreateK8sObject(&rolebinding, gvr, rbacNSName, filepath.Join(rbacFolderPath, "compositiondefinition-rolebinding.yaml"), "serviceAccount", sa.Name, "saNamespace", sa.Namespace)
	if err != nil {
		return corev1.ServiceAccount{}, rbacv1.ClusterRole{}, rbacv1.ClusterRoleBinding{}, rbacv1.Role{}, rbacv1.RoleBinding{}, err
	}

	return sa, clusterrole, clusterrolebinding, role, rolebinding, nil
}

func installRBACResources(ctx context.Context, dyn dynamic.Interface, clusterrole rbacv1.ClusterRole, clusterrolebinding rbacv1.ClusterRoleBinding, role rbacv1.Role, rolebinding rbacv1.RoleBinding, sa corev1.ServiceAccount, hsh *hasher.ObjectHash, applyOpts kubecli.ApplyOptions) error {
	if hsh == nil {
		return fmt.Errorf("hasher is required")
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())
	err := kubecli.Apply(ctx, dyn, clusterRoleGVR, &clusterrole, applyOpts)
	if err != nil {
		log.Error(err, "installing clusterrole", "name", clusterrole.Name, "namespace", clusterrole.Namespace)
		return err
	}

	err = hsh.SumHash(clusterrole.ObjectMeta.Name, clusterrole.ObjectMeta.Namespace, clusterrole.Rules)
	if err != nil {
		return fmt.Errorf("error hashing clusterrole: %v", err)
	}
	log.Debug("ClusterRole successfully hashed", "name", clusterrole.Name, "namespace", clusterrole.Namespace, "digest", hsh.GetHash())

	err = kubecli.Apply(ctx, dyn, clusterRoleBindingGVR, &clusterrolebinding, applyOpts)
	if err != nil {
		log.Error(err, "installing clusterrolebinding", "name", clusterrolebinding.Name, "namespace", clusterrolebinding.Namespace)
		return err
	}
	err = hsh.SumHash(clusterrolebinding.ObjectMeta.Name, clusterrolebinding.ObjectMeta.Namespace, clusterrolebinding.Subjects, clusterrolebinding.RoleRef)
	if err != nil {
		return fmt.Errorf("error hashing clusterrolebinding: %v", err)
	}
	log.Debug("ClusterRoleBinding successfully installed", "name", clusterrolebinding.Name, "namespace", clusterrolebinding.Namespace, "digest", hsh.GetHash())

	err = kubecli.Apply(ctx, dyn, roleGVR, &role, applyOpts)
	if err != nil {
		log.Error(err, "installing role", "name", role.Name, "namespace", role.Namespace)
		return err
	}
	err = hsh.SumHash(role.ObjectMeta.Name, role.ObjectMeta.Namespace, role.Rules)
	if err != nil {
		return fmt.Errorf("error hashing role: %v", err)
	}
	log.Debug("Role successfully installed", "name", role.Name, "namespace", role.Namespace, "digest", hsh.GetHash())

	err = kubecli.Apply(ctx, dyn, roleBindingGVR, &rolebinding, applyOpts)
	if err != nil {
		log.Error(err, "installing rolebinding", "name", rolebinding.Name, "namespace", rolebinding.Namespace)
		return err
	}
	err = hsh.SumHash(rolebinding.ObjectMeta.Name, rolebinding.ObjectMeta.Namespace, rolebinding.Subjects, rolebinding.RoleRef)
	if err != nil {
		return fmt.Errorf("error hashing rolebinding: %v", err)
	}

	log.Debug("RoleBinding successfully installed", "name", rolebinding.Name, "namespace", rolebinding.Namespace, "digest", hsh.GetHash())

	err = kubecli.Apply(ctx, dyn, serviceAccountGVR, &sa, applyOpts)
	if err != nil {
		log.Error(err, "installing serviceaccount", "name", sa.Name, "namespace", sa.Namespace)
		return err
	}
	err = hsh.SumHash(sa.ObjectMeta.Name, sa.ObjectMeta.Namespace)
	if err != nil {
		return fmt.Errorf("error hashing serviceaccount: %v", err)
	}
	log.Debug("ServiceAccount successfully installed", "name", sa.Name, "namespace", sa.Namespace, "digest", hsh.GetHash())

	return nil
}

func lookupRBACResources(ctx context.Context, dyn dynamic.Interface, clusterrole rbacv1.ClusterRole, clusterrolebinding rbacv1.ClusterRoleBinding, role rbacv1.Role, rolebinding rbacv1.RoleBinding, sa corev1.ServiceAccount, hsh *hasher.ObjectHash) error {
	if hsh == nil {
		return fmt.Errorf("hasher is required")
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	err := kubecli.Get(ctx, dyn, clusterRoleGVR, &clusterrole)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("ClusterRole not found", "name", clusterrole.Name, "namespace", clusterrole.Namespace)
			clusterrole = rbacv1.ClusterRole{}
		} else {
			return fmt.Errorf("error getting clusterrole: %w", err)
		}
	}
	err = hsh.SumHash(clusterrole.ObjectMeta.Name, clusterrole.ObjectMeta.Namespace, clusterrole.Rules)
	if err != nil {
		return fmt.Errorf("error hashing clusterrole: %v", err)
	}
	log.Debug("ClusterRole successfully fetched", "name", clusterrole.Name, "namespace", clusterrole.Namespace, "digest", hsh.GetHash())

	err = kubecli.Get(ctx, dyn, clusterRoleBindingGVR, &clusterrolebinding)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("ClusterRoleBinding not found", "name", clusterrolebinding.Name, "namespace", clusterrolebinding.Namespace)
			clusterrolebinding = rbacv1.ClusterRoleBinding{}
		} else {
			return fmt.Errorf("error getting clusterrolebinding: %w", err)
		}
	}
	err = hsh.SumHash(clusterrolebinding.ObjectMeta.Name, clusterrolebinding.ObjectMeta.Namespace, clusterrolebinding.Subjects, clusterrolebinding.RoleRef)
	if err != nil {
		return fmt.Errorf("error hashing clusterrolebinding: %v", err)
	}
	log.Debug("ClusterRoleBinding successfully fetched", "name", clusterrolebinding.Name, "namespace", clusterrolebinding.Namespace, "digest", hsh.GetHash())

	err = kubecli.Get(ctx, dyn, roleGVR, &role)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("Role not found", "name", role.Name, "namespace", role.Namespace)
			role = rbacv1.Role{}
		} else {
			return fmt.Errorf("error getting role: %w", err)
		}
	}
	err = hsh.SumHash(role.ObjectMeta.Name, role.ObjectMeta.Namespace, role.Rules)
	if err != nil {
		return fmt.Errorf("error hashing role: %v", err)
	}
	log.Debug("Role successfully fetched", "name", role.Name, "namespace", role.Namespace, "digest", hsh.GetHash())

	err = kubecli.Get(ctx, dyn, roleBindingGVR, &rolebinding)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("RoleBinding not found", "name", rolebinding.Name, "namespace", rolebinding.Namespace)
			rolebinding = rbacv1.RoleBinding{}
		} else {
			return fmt.Errorf("error getting rolebinding: %w", err)
		}
	}
	err = hsh.SumHash(rolebinding.ObjectMeta.Name, rolebinding.ObjectMeta.Namespace, rolebinding.Subjects, rolebinding.RoleRef)
	if err != nil {
		return fmt.Errorf("error hashing rolebinding: %v", err)
	}
	log.Debug("RoleBinding successfully fetched", "name", rolebinding.Name, "namespace", rolebinding.Namespace, "digest", hsh.GetHash())

	err = kubecli.Get(ctx, dyn, serviceAccountGVR, &sa)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("ServiceAccount not found", "name", sa.Name, "namespace", sa.Namespace)
			sa = corev1.ServiceAccount{}
		} else {
			return fmt.Errorf("error getting serviceaccount: %w", err)
		}
	}
	err = hsh.SumHash(sa.ObjectMeta.Name, sa.ObjectMeta.Namespace)
	if err != nil {
		return fmt.Errorf("error hashing serviceaccount: %v", err)
	}
	log.Debug("ServiceAccount successfully fetched", "name", sa.Name, "namespace", sa.Namespace, "digest", hsh.GetHash())

	return nil
}

func uninstallRBACResources(ctx context.Context, dyn dynamic.Interface, clusterrole rbacv1.ClusterRole, clusterrolebinding rbacv1.ClusterRoleBinding, role rbacv1.Role, rolebinding rbacv1.RoleBinding, sa corev1.ServiceAccount, keepClusterScoped bool) error {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	if keepClusterScoped {
		log.Debug("Keeping shared cluster-scoped RBAC: another definition still uses this controller version",
			"clusterrole", clusterrole.Name, "clusterrolebinding", clusterrolebinding.Name)
	} else {
		err := kubecli.Uninstall(ctx, dyn, clusterRoleGVR, &clusterrole, kubecli.UninstallOptions{})
		if err != nil {
			log.Error(err, "Error uninstalling clusterrole", "name", clusterrole.Name, "namespace", clusterrole.Namespace)
			return err
		}
		log.Debug("ClusterRole successfully uninstalled", "name", clusterrole.Name, "namespace", clusterrole.Namespace)

		err = kubecli.Uninstall(ctx, dyn, clusterRoleBindingGVR, &clusterrolebinding, kubecli.UninstallOptions{})
		if err != nil {
			log.Error(err, "Error uninstalling clusterrolebinding", "name", clusterrolebinding.Name, "namespace", clusterrolebinding.Namespace)
			return err
		}
		log.Debug("ClusterRoleBinding successfully uninstalled", "name", clusterrolebinding.Name, "namespace", clusterrolebinding.Namespace)
	}

	err := kubecli.Uninstall(ctx, dyn, roleGVR, &role, kubecli.UninstallOptions{})
	if err != nil {
		log.Error(err, "uninstalling role", "name", role.Name, "namespace", role.Namespace)
		return err
	}
	log.Debug("Role successfully uninstalled", "name", role.Name, "namespace", role.Namespace)

	err = kubecli.Uninstall(ctx, dyn, roleBindingGVR, &rolebinding, kubecli.UninstallOptions{})
	if err != nil {
		log.Error(err, "uninstalling rolebinding", "name", rolebinding.Name, "namespace", rolebinding.Namespace)
		return err
	}
	log.Debug("RoleBinding successfully uninstalled", "name", rolebinding.Name, "namespace", rolebinding.Namespace)

	err = kubecli.Uninstall(ctx, dyn, serviceAccountGVR, &sa, kubecli.UninstallOptions{})
	if err != nil {
		log.Error(err, "uninstalling serviceaccount", "name", sa.Name, "namespace", sa.Namespace)
		return err
	}
	log.Debug("ServiceAccount successfully uninstalled", "name", sa.Name, "namespace", sa.Namespace)

	return nil
}

func Deploy(ctx context.Context, kube client.Client, opts DeployOptions) (digest string, err error) {
	applyOpts := kubecli.ApplyOptions{}

	namespacedName := types.NamespacedName{
		Namespace: opts.Namespace,
		Name:      resourceNamer(opts.GVR.Resource, opts.GVR.Version),
	}

	if opts.DryRunServer {
		applyOpts.DryRun = []string{"All"}
	}

	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	sa, clusterrole, clusterrolebinding, role, rolebinding, err := createRBACResources(opts.GVR, getCDCrbacNN(namespacedName), opts.RBACFolderPath)
	if err != nil {
		return "", err
	}

	hsh := hasher.NewFNVObjectHash()
	if opts.Spec != nil && opts.Spec.Credentials != nil {
		role := rbacv1.Role{}
		err = objects.CreateK8sObject(&role,
			opts.GVR,
			getCDCrbacNN(types.NamespacedName{Namespace: opts.Spec.Credentials.PasswordRef.Namespace, Name: namespacedName.Name}),
			filepath.Join(opts.RBACFolderPath, "secret-role.yaml"),
			"secretName", opts.Spec.Credentials.PasswordRef.Name)
		if err != nil {
			log.Error(err, "creating role")
			return "", err
		}

		err = kubecli.Apply(ctx, opts.DynClient, roleGVR, &role, applyOpts)
		if err != nil {
			log.Error(err, "installing role")
			return "", err
		}
		err = hsh.SumHash(role.ObjectMeta.Name, role.ObjectMeta.Namespace, role.Rules)
		if err != nil {
			return "", fmt.Errorf("error hashing role: %v", err)
		}

		log.Debug("Role successfully hashed", "gvr", opts.GVR.String(), "name", role.Name, "namespace", role.Namespace, "digest", hsh.GetHash())

		rolebinding := rbacv1.RoleBinding{}
		err := objects.CreateK8sObject(&rolebinding,
			opts.GVR,
			getCDCrbacNN(types.NamespacedName{Namespace: opts.Spec.Credentials.PasswordRef.Namespace, Name: namespacedName.Name}),
			filepath.Join(opts.RBACFolderPath, "secret-rolebinding.yaml"),
			"serviceAccount", sa.Name,
			"saNamespace", sa.Namespace)
		if err != nil {
			log.Error(err, "creating rolebinding")
			return "", err
		}

		err = kubecli.Apply(ctx, opts.DynClient, roleBindingGVR, &rolebinding, applyOpts)
		if err != nil {
			log.Error(err, "installing rolebinding")
			return "", err
		}
		err = hsh.SumHash(rolebinding.ObjectMeta.Name, rolebinding.ObjectMeta.Namespace, rolebinding.Subjects, rolebinding.RoleRef)
		if err != nil {
			return "", fmt.Errorf("error hashing rolebinding: %v", err)
		}
		log.Debug("RoleBinding successfully installed", "gvr", opts.GVR.String(), "name", rolebinding.Name, "namespace", rolebinding.Namespace, "digest", hsh.GetHash())
	}

	err = installRBACResources(ctx, opts.DynClient, clusterrole, clusterrolebinding, role, rolebinding, sa, &hsh, applyOpts)
	if err != nil {
		return "", err
	}

	// When the CompositionDefinition declares an apiRef, register the CDC ServiceAccount in
	// authn's allowlist so it can exchange its projected token for a service JWT.
	err = applyAuthnServiceAccountMapping(ctx, opts.DynClient, opts, sa.Name, sa.Namespace, &hsh, applyOpts)
	if err != nil {
		return "", err
	}

	// Ensure core-provider's OWN authn mapping exists before calling /rbac (which authenticates
	// with core-provider's projected token). Provisioned at runtime — lazily, once authn is up —
	// rather than declaratively at bootstrap, where the authn CRD does not yet exist.
	err = ensureSelfAuthnMapping(ctx, opts.DynClient, opts, applyOpts)
	if err != nil {
		return "", err
	}

	// ...and grant the per-composition group exactly the in-cluster reads the referenced
	// RESTAction performs (snowplow GET /rbac). Returns restactionrbac.ErrIncomplete on a 422 —
	// the caller maps that to the ApiRefRBACIncomplete condition instead of writing partial RBAC.
	err = applyRestActionRBAC(ctx, opts.DynClient, opts, sa.Name, &hsh, applyOpts)
	if err != nil {
		return "", err
	}

	jsonSchemaConfigmap := corev1.ConfigMap{}
	err = objects.CreateK8sObject(&jsonSchemaConfigmap, opts.GVR, getJsonSchemaConfigmapNN(namespacedName), opts.JsonSchemaTemplatePath,
		"schema", string(opts.JsonSchemaBytes),
	)
	if err != nil {
		return "", fmt.Errorf("error creating ConfigMap for JSON schema: %w", err)
	}
	err = kubecli.Apply(ctx, opts.DynClient, configMapGVR, &jsonSchemaConfigmap, applyOpts)
	if err != nil {
		return "", fmt.Errorf("error applying ConfigMap for JSON schema: %w", err)
	}
	err = hsh.SumHash(jsonSchemaConfigmap.ObjectMeta.Name, jsonSchemaConfigmap.ObjectMeta.Namespace)
	if err != nil {
		return "", fmt.Errorf("error hashing JSON schema configmap: %v", err)
	}
	log.Debug("JSON Schema ConfigMap successfully installed", "gvr", opts.GVR.String(), "name", jsonSchemaConfigmap.Name, "namespace", jsonSchemaConfigmap.Namespace, "digest", hsh.GetHash())

	cm := corev1.ConfigMap{}
	err = objects.CreateK8sObject(&cm, opts.GVR, getCDCConfigmapNN(namespacedName), opts.ConfigmapTemplatePath,
		"composition_controller_sa_name", sa.Name,
		"composition_controller_sa_namespace", sa.Namespace,
		"status_data_template", opts.StatusDataTemplate,
		"api_ref_name", opts.ApiRefName,
		"api_ref_namespace", opts.ApiRefNamespace,
		"api_ref_extras", opts.ApiRefExtras)
	if err != nil {
		return "", err
	}
	err = kubecli.Apply(ctx, opts.DynClient, configMapGVR, &cm, applyOpts)
	if err != nil {
		log.Error(err, "installing configmap")
		return "", err
	}
	err = hsh.SumHash(cm.ObjectMeta.Name, cm.ObjectMeta.Namespace, cm.Data)
	if err != nil {
		return "", fmt.Errorf("error hashing configmap: %v", err)
	}
	log.Debug("Configmap successfully installed", "gvr", opts.GVR.String(), "name", cm.Name, "namespace", cm.Namespace, "digest", hsh.GetHash())

	dep := appsv1.Deployment{}
	// Per-Kind CDC tuning: pass spec.controller values only when set, so the Deployment template's
	// chart-global defaults (workers=1, resync=3m) apply for definitions that omit them. Rendering
	// here (Deploy) is what gets applied + hashed; the Lookup render is overwritten by a live Get,
	// so it needs no change — a spec.controller edit re-renders, re-applies, and re-hashes here,
	// which is exactly what triggers the CDC restart.
	depValues := []any{"serviceAccountName", sa.Name, "api_ref_name", opts.ApiRefName}
	if c := opts.Controller; c != nil {
		if c.Workers != nil {
			depValues = append(depValues, "workers", *c.Workers)
		}
		if c.ResyncInterval != nil {
			depValues = append(depValues, "resyncInterval", *c.ResyncInterval)
		}
		if c.Resources != nil {
			// Marshal to JSON (valid YAML) and inline it into the template as `resources: {...}`.
			rj, err := json.Marshal(c.Resources)
			if err != nil {
				return "", fmt.Errorf("marshaling spec.controller.resources: %w", err)
			}
			depValues = append(depValues, "resources", string(rj))
		}
	}
	err = objects.CreateK8sObject(
		&dep,
		opts.GVR,
		getCDCDeploymentNN(namespacedName),
		opts.DeploymentTemplatePath,
		depValues...)
	if err != nil {
		return "", err
	}
	err = kubecli.Apply(ctx, opts.DynClient, deploymentGVR, &dep, applyOpts)
	if err != nil {
		log.Error(err, "installing deployment")
		return "", err
	}

	deployment.CleanFromRestartAnnotation(&dep)

	err = hsh.SumHash(dep.ObjectMeta.Name, dep.ObjectMeta.Namespace, dep.Spec)
	if err != nil {
		return "", fmt.Errorf("error hashing deployment spec: %v", err)
	}
	log.Debug("Deployment successfully installed", "gvr", opts.GVR.String(), "name", dep.Name, "namespace", dep.Namespace, "digest", hsh.GetHash())

	_, err = os.Stat(opts.ServiceTemplatePath)
	if err == nil {
		svc := corev1.Service{}
		err = objects.CreateK8sObject(&svc, opts.GVR, getCDCDeploymentNN(namespacedName), opts.ServiceTemplatePath)
		if err != nil {
			log.Error(err, "creating service")
			return "", err
		}

		err = kubecli.Apply(ctx, opts.DynClient, serviceGVR, &svc, applyOpts)
		if err != nil {
			log.Error(err, "installing service")
			return "", err
		}
		err = hsh.SumHash(svc.ObjectMeta.Name, svc.ObjectMeta.Namespace, svc.Spec)
		if err != nil {
			return "", fmt.Errorf("error hashing service: %v", err)
		}
		log.Debug("Service successfully installed", "gvr", opts.GVR.String(), "name", svc.Name, "namespace", svc.Namespace, "digest", hsh.GetHash())
	}

	if !opts.DryRunServer {
		// Wait for deployment to be ready after creation
		err := watcher.NewWatcher(opts.DynClient,
			appsv1.SchemeGroupVersion.WithResource("deployments"),
			1*time.Minute,
			deployment.IsReady).WatchResource(ctx, dep.GetNamespace(), dep.GetName())
		if err != nil {
			log.Error(err, "waiting for deployment to be ready")
			return "", fmt.Errorf("error waiting for deployment to be ready: %w", err)
		}

		// Deployment needs to be restarted if the hash changes to get the new configmap
		err = deployment.RestartDeployment(ctx, opts.KubeClient, &dep)
		if err != nil {
			log.Error(err, "restarting deployment")
			return "", err
		}

		// Wait for deployment to be ready after restart
		err = watcher.NewWatcher(opts.DynClient,
			appsv1.SchemeGroupVersion.WithResource("deployments"),
			1*time.Minute,
			deployment.IsReady).WatchResource(ctx, dep.GetNamespace(), dep.GetName())
		if err != nil {
			log.Error(err, "waiting for deployment to be ready")
			return "", fmt.Errorf("waiting for deployment to be ready: %w", err)
		}
	}

	return hsh.GetHash(), nil
}

func Undeploy(ctx context.Context, kube client.Client, opts UndeployOptions) error {
	namespacedName := types.NamespacedName{
		Namespace: opts.Namespace,
		Name:      resourceNamer(opts.GVR.Resource, opts.GVR.Version),
	}

	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	// Safety net: when this teardown will delete the CRD (!SkipCRD), never proceed while
	// live composition instances still exist. The controller drains instances per-version
	// using the krateo.io/composition-version label (stamped by a MutatingAdmissionPolicy);
	// if that policy is absent in the target the label-filtered drain is a silent no-op, so
	// guard here with an UNFILTERED list before tearing anything down. Refusing up front
	// keeps the CDC alive to finalize those instances instead of orphaning them, and
	// prevents the CRD deletion from cascade-garbage-collecting every instance.
	if !opts.SkipCRD {
		remaining, err := opts.DynamicClient.Resource(opts.GVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("error listing composition instances before teardown: %w", err)
		}
		if len(remaining.Items) > 0 {
			log.Debug("Refusing to undeploy while composition instances still exist",
				"gvr", opts.GVR.String(), "count", len(remaining.Items))
			return ErrCompositionStillExist
		}
	}

	sa, clusterrole, clusterrolebinding, role, rolebinding, err := createRBACResources(opts.GVR, getCDCrbacNN(namespacedName), opts.RBACFolderPath)
	if err != nil {
		return err
	}

	jsonSchemaConfigmap := corev1.ConfigMap{}
	err = objects.CreateK8sObject(&jsonSchemaConfigmap, opts.GVR, getJsonSchemaConfigmapNN(namespacedName), opts.JsonSchemaTemplatePath,
		"schema", string(opts.JsonSchemaBytes),
	)
	if err != nil {
		return fmt.Errorf("error creating ConfigMap for JSON schema: %w", err)
	}
	err = kubecli.Uninstall(ctx, opts.DynamicClient, configMapGVR, &jsonSchemaConfigmap, kubecli.UninstallOptions{})
	if err != nil {
		log.Error(err, "Error uninstalling ConfigMap for JSON schema")
		return err
	}
	log.Debug("JSON Schema ConfigMap successfully uninstalled", "gvr", opts.GVR.String(), "name", jsonSchemaConfigmap.Name, "namespace", jsonSchemaConfigmap.Namespace)

	dep := appsv1.Deployment{}
	err = objects.CreateK8sObject(
		&dep,
		opts.GVR,
		getCDCDeploymentNN(namespacedName),
		opts.DeploymentTemplatePath,
		"serviceAccountName", sa.Name,
		// Undeploy renders the Deployment only to delete it by name; the token volume is
		// irrelevant here.
		"api_ref_name", "")
	if err != nil {
		return err
	}

	err = kubecli.Uninstall(ctx, opts.DynamicClient, deploymentGVR, &dep, kubecli.UninstallOptions{})
	if err != nil {
		log.Error(err, "Error uninstalling deployment", "gvr", opts.GVR.String(), "name", dep.Name, "namespace", dep.Namespace)
		return err
	}
	log.Debug("Deployment successfully uninstalled", "gvr", opts.GVR.String(), "name", dep.Name, "namespace", dep.Namespace)

	// #263. The Uninstall above returns when the apiserver ACCEPTS the delete. The pod is then
	// Terminating and has begun its graceful-shutdown drain — unstructured-runtime keeps it alive
	// for GracefulShutdownTimeout (30s by default) precisely so an in-flight reconcile can finish.
	// Everything below this point deletes that pod's ServiceAccount, RBAC and authn mapping, so
	// without this wait the drain window survives and the IDENTITY does not: the controller spends
	// 30s unable to read or write anything, and whatever it still owed is lost.
	//
	// That is how a composition ends up recording a resource the release no longer renders. A
	// version migration killed the outgoing controller between its helm upgrade and its status
	// write, and status.managed kept the old name (#261).
	//
	// Two protections already existed and neither covers this. The runtime's drain window is one;
	// the chart's cdc.terminationGracePeriodSeconds (45s) is the other, and its comment says it
	// "must exceed the graceful-shutdown drain window so the kubelet does not SIGKILL mid-drain".
	// Both are correct. They defend against the KUBELET. Nothing defended against the controller
	// that requested the deletion revoking the identity the drain depends on.
	if err := waitForControllerDrained(ctx, opts.KubeClient, &dep, opts.DrainTimeout); err != nil {
		// Deliberately fatal rather than falling through. "I could not confirm the pod is gone" is
		// exactly the unverifiable case where proceeding is the destructive choice: revoking an
		// identity from under a controller that is still reconciling loses writes, while leaving a
		// ServiceAccount behind costs nothing and the next reconcile retries idempotently.
		log.Error(err, "Refusing to revoke the retiring controller's identity before it has drained",
			"gvr", opts.GVR.String(), "name", dep.Name, "namespace", dep.Namespace)
		return fmt.Errorf("waiting for retiring controller to drain: %w", err)
	}

	cm := corev1.ConfigMap{}
	err = objects.CreateK8sObject(&cm, opts.GVR, getCDCConfigmapNN(namespacedName), opts.ConfigmapTemplatePath,
		"composition_controller_sa_name", sa.Name,
		"composition_controller_sa_namespace", sa.Namespace,
		"status_data_template", "",
		"api_ref_name", "",
		"api_ref_namespace", "",
		"api_ref_extras", "")
	if err != nil {
		return err
	}

	err = kubecli.Uninstall(ctx, opts.DynamicClient, configMapGVR, &cm, kubecli.UninstallOptions{})
	if err != nil {
		log.Error(err, "uninstalling configmap")
		return err
	}
	log.Debug("Configmap successfully uninstalled", "gvr", opts.GVR.String(), "name", cm.Name, "namespace", cm.Namespace)

	err = uninstallRBACResources(ctx, opts.DynamicClient, clusterrole, clusterrolebinding, role, rolebinding, sa, opts.KeepShared)
	if err != nil {
		return err
	}

	// Remove the authn allowlist mapping (no-op if absent / authn CRD not installed). Done
	// unconditionally so it is cleaned up even if apiRef was removed before undeploy.
	err = deleteAuthnServiceAccountMapping(ctx, opts.KubeClient, opts.AuthnNamespace, opts.Namespace, sa.Name)
	if err != nil {
		return err
	}

	// Remove the apiRef RBAC (ClusterRole + binding) by name; not-found is tolerated. Its name has
	// no namespace in it, so it is shared with any other definition on this Kind+version.
	if !opts.KeepShared {
		err = deleteRestActionRBAC(ctx, opts.KubeClient, sa.Name)
		if err != nil {
			return err
		}
	}

	_, err = os.Stat(opts.ServiceTemplatePath)
	if err == nil {
		svc := corev1.Service{}
		err = objects.CreateK8sObject(&svc, opts.GVR, getCDCDeploymentNN(namespacedName), opts.ServiceTemplatePath)
		if err != nil {
			log.Error(err, "creating service")
			return err
		}

		err = kubecli.Uninstall(ctx, opts.DynamicClient, serviceGVR, &svc, kubecli.UninstallOptions{})
		if err != nil {
			log.Error(err, "uninstalling service")
			return err
		}
		log.Debug("Service successfully uninstalled", "gvr", opts.GVR.String(), "name", svc.Name, "namespace", svc.Namespace)
	}

	// The credentials Role/RoleBinding live in the Secret's namespace under the shared
	// {resource}-{apiVersion} name; outside this definition's namespace another definition on the
	// same version may be using them.
	keepCredentialsRBAC := opts.KeepShared && opts.Spec != nil && opts.Spec.Credentials != nil &&
		opts.Spec.Credentials.PasswordRef.Namespace != opts.Namespace
	if opts.Spec != nil && opts.Spec.Credentials != nil && !keepCredentialsRBAC {
		role := rbacv1.Role{}
		err = objects.CreateK8sObject(&role, opts.GVR, getCDCrbacNN(types.NamespacedName{Namespace: opts.Spec.Credentials.PasswordRef.Namespace, Name: namespacedName.Name}), filepath.Join(opts.RBACFolderPath, "secret-role.yaml"), "secretName", opts.Spec.Credentials.PasswordRef.Name)
		if err != nil {
			log.Error(err, "creating role")
			return err
		}

		err = kubecli.Uninstall(ctx, opts.DynamicClient, roleGVR, &role, kubecli.UninstallOptions{})
		if err != nil {
			log.Error(err, "uninstalling role")
			return err
		}
		log.Debug("Role successfully uninstalled", "gvr", opts.GVR.String(), "name", role.Name, "namespace", role.Namespace)

		rolebinding := rbacv1.RoleBinding{}
		err = objects.CreateK8sObject(&rolebinding, opts.GVR, getCDCrbacNN(types.NamespacedName{Namespace: opts.Spec.Credentials.PasswordRef.Namespace, Name: namespacedName.Name}), filepath.Join(opts.RBACFolderPath, "secret-rolebinding.yaml"))
		if err != nil {
			log.Error(err, "creating rolebinding")
			return err
		}

		err = kubecli.Uninstall(ctx, opts.DynamicClient, roleBindingGVR, &rolebinding, kubecli.UninstallOptions{})
		if err != nil {
			log.Error(err, "uninstalling rolebinding")
			return err
		}
		log.Debug("RoleBinding successfully uninstalled", "gvr", opts.GVR.String(), "name", rolebinding.Name, "namespace", rolebinding.Namespace)
	}

	log.Debug("RBAC resources successfully uninstalled", "gvr", opts.GVR.String())

	if !opts.SkipCRD {
		err := crd.Uninstall(ctx, opts.KubeClient, opts.GVR.GroupResource())
		if err != nil {
			log.Debug("Error uninstalling CRD", "name", opts.GVR.GroupResource().String(), "error", err)
			return err
		}
		log.Debug("CRD successfully uninstalled", "name", opts.GVR.GroupResource().String())
	}

	return nil
}

// This function is used to lookup the current state of the deployment and return the hash of the current state
// This is used to determine if the deployment needs to be updated or not
func Lookup(ctx context.Context, kube client.Client, opts DeployOptions) (digest string, err error) {
	namespacedName := types.NamespacedName{
		Namespace: opts.Namespace,
		Name:      resourceNamer(opts.GVR.Resource, opts.GVR.Version),
	}

	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	sa, clusterrole, clusterrolebinding, role, rolebinding, err := createRBACResources(opts.GVR, getCDCrbacNN(namespacedName), opts.RBACFolderPath)
	if err != nil {
		return "", err
	}

	hsh := hasher.NewFNVObjectHash()
	if opts.Spec != nil && opts.Spec.Credentials != nil {
		role := rbacv1.Role{}
		err = objects.CreateK8sObject(&role, opts.GVR, getCDCrbacNN(types.NamespacedName{Namespace: opts.Spec.Credentials.PasswordRef.Namespace, Name: namespacedName.Name}), filepath.Join(opts.RBACFolderPath, "secret-role.yaml"), "secretName", opts.Spec.Credentials.PasswordRef.Name)
		if err != nil {
			log.Error(err, "creating role")
			return "", err
		}

		err = kubecli.Get(ctx, opts.DynClient, roleGVR, &role)
		if err != nil {
			log.Error(err, "fetching role")
			return "", err
		}
		err = hsh.SumHash(role.ObjectMeta.Name, role.ObjectMeta.Namespace, role.Rules)
		if err != nil {
			return "", fmt.Errorf("error hashing role: %v", err)
		}
		log.Debug("Role successfully fetched", "gvr", opts.GVR.String(), "name", role.Name, "namespace", role.Namespace, "digest", hsh.GetHash())

		rolebinding := rbacv1.RoleBinding{}
		err = objects.CreateK8sObject(&rolebinding, opts.GVR, getCDCrbacNN(types.NamespacedName{Namespace: opts.Spec.Credentials.PasswordRef.Namespace, Name: namespacedName.Name}), filepath.Join(opts.RBACFolderPath, "secret-rolebinding.yaml"), "serviceAccount", sa.Name, "saNamespace", sa.Namespace)
		if err != nil {
			log.Error(err, "creating rolebinding")
			return "", err
		}

		err = kubecli.Get(ctx, opts.DynClient, roleBindingGVR, &rolebinding)
		if err != nil {
			log.Error(err, "fetching rolebinding")
			return "", err
		}
		err = hsh.SumHash(rolebinding.ObjectMeta.Name, rolebinding.ObjectMeta.Namespace, rolebinding.Subjects, rolebinding.RoleRef)
		if err != nil {
			return "", fmt.Errorf("error hashing rolebinding: %v", err)
		}
		log.Debug("RoleBinding successfully fetched", "gvr", opts.GVR.String(), "name", rolebinding.Name, "namespace", rolebinding.Namespace, "digest", hsh.GetHash())
	}

	err = lookupRBACResources(ctx, opts.DynClient, clusterrole, clusterrolebinding, role, rolebinding, sa, &hsh)
	if err != nil {
		return "", err
	}

	// Contribute the authn mapping to the digest (deterministically) so it matches Deploy.
	if err = hashAuthnServiceAccountMapping(opts, sa.Name, &hsh); err != nil {
		return "", fmt.Errorf("error hashing authn ServiceAccount mapping: %v", err)
	}

	// Live apiRef RBAC contribution — read the ClusterRole + binding and hash, mirroring the
	// rendered-side applyRestActionRBAC so a deleted/edited grant surfaces as drift. No snowplow
	// call here; only Deploy queries /rbac.
	if err = lookupRestActionRBAC(ctx, opts.KubeClient, opts, sa.Name, &hsh); err != nil {
		return "", fmt.Errorf("error hashing apiRef RBAC: %v", err)
	}

	jsonSchemaConfigmap := corev1.ConfigMap{}
	err = objects.CreateK8sObject(&jsonSchemaConfigmap, opts.GVR, getJsonSchemaConfigmapNN(namespacedName), opts.JsonSchemaTemplatePath,
		"schema", string(opts.JsonSchemaBytes),
	)
	if err != nil {
		return "", fmt.Errorf("error creating ConfigMap for JSON schema: %w", err)
	}
	err = kubecli.Get(ctx, opts.DynClient, configMapGVR, &jsonSchemaConfigmap)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("JSON Schema ConfigMap not found", "gvr", opts.GVR.String(), "name", jsonSchemaConfigmap.Name, "namespace", jsonSchemaConfigmap.Namespace)
			jsonSchemaConfigmap = corev1.ConfigMap{}
		} else {
			log.Error(err, "fetching ConfigMap for JSON schema")
			return "", fmt.Errorf("error fetching ConfigMap for JSON schema: %w", err)
		}
	}
	err = hsh.SumHash(jsonSchemaConfigmap.ObjectMeta.Name, jsonSchemaConfigmap.ObjectMeta.Namespace)
	if err != nil {
		return "", fmt.Errorf("error hashing JSON schema configmap: %v", err)
	}
	log.Debug("JSON Schema ConfigMap successfully fetched", "gvr", opts.GVR.String(), "name", jsonSchemaConfigmap.Name, "namespace", jsonSchemaConfigmap.Namespace, "digest", hsh.GetHash())

	cm := corev1.ConfigMap{}
	err = objects.CreateK8sObject(&cm, opts.GVR, getCDCConfigmapNN(namespacedName), opts.ConfigmapTemplatePath,
		"composition_controller_sa_name", sa.Name,
		"composition_controller_sa_namespace", sa.Namespace,
		"status_data_template", opts.StatusDataTemplate,
		"api_ref_name", opts.ApiRefName,
		"api_ref_namespace", opts.ApiRefNamespace,
		"api_ref_extras", opts.ApiRefExtras)
	if err != nil {
		return "", err
	}
	err = kubecli.Get(ctx, opts.DynClient, configMapGVR, &cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("Configmap not found", "gvr", opts.GVR.String(), "name", cm.Name, "namespace", cm.Namespace)
			cm = corev1.ConfigMap{}
		} else {
			log.Error(err, "fetching configmap")
			return "", fmt.Errorf("error fetching configmap: %w", err)
		}
	}
	err = hsh.SumHash(cm.ObjectMeta.Name, cm.ObjectMeta.Namespace, cm.Data)
	if err != nil {
		return "", fmt.Errorf("error hashing configmap: %v", err)
	}
	log.Debug("Configmap successfully fetched", "gvr", opts.GVR.String(), "name", cm.Name, "namespace", cm.Namespace, "digest", hsh.GetHash())

	dep := appsv1.Deployment{}
	err = objects.CreateK8sObject(
		&dep,
		opts.GVR,
		getCDCDeploymentNN(namespacedName),
		opts.DeploymentTemplatePath,
		"serviceAccountName", sa.Name,
		"api_ref_name", opts.ApiRefName)
	if err != nil {
		return "", err
	}

	err = kubecli.Get(ctx, opts.DynClient, deploymentGVR, &dep)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("Deployment not found", "gvr", opts.GVR.String(), "name", dep.Name, "namespace", dep.Namespace)
			dep = appsv1.Deployment{}
		} else {
			log.Error(err, "fetching deployment")
			return "", fmt.Errorf("error fetching deployment: %w", err)
		}
	}

	deployment.CleanFromRestartAnnotation(&dep)

	err = hsh.SumHash(dep.ObjectMeta.Name, dep.ObjectMeta.Namespace, dep.Spec)
	if err != nil {
		return "", fmt.Errorf("error hashing deployment spec: %v", err)
	}
	log.Debug("Deployment successfully fetched", "gvr", opts.GVR.String(), "name", dep.Name, "namespace", dep.Namespace, "digest", hsh.GetHash())

	_, err = os.Stat(opts.ServiceTemplatePath)
	if err == nil {
		svc := corev1.Service{}
		err = objects.CreateK8sObject(&svc, opts.GVR, getCDCDeploymentNN(namespacedName), opts.ServiceTemplatePath)
		if err != nil {
			log.Error(err, "creating service")
			return "", err
		}
		err = kubecli.Get(ctx, opts.DynClient, serviceGVR, &svc)
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Debug("Service not found", "gvr", opts.GVR.String(), "name", svc.Name, "namespace", svc.Namespace)
				svc = corev1.Service{}
			} else {
				log.Error(err, "fetching service")
				return "", fmt.Errorf("error fetching service: %w", err)
			}
		}
		err = hsh.SumHash(svc.ObjectMeta.Name, svc.ObjectMeta.Namespace, svc.Spec)
		if err != nil {
			return "", fmt.Errorf("error hashing service: %v", err)
		}
		log.Debug("Service successfully fetched", "gvr", opts.GVR.String(), "name", svc.Name, "namespace", svc.Namespace, "digest", hsh.GetHash())
	}

	return hsh.GetHash(), nil
}

func getCDCConfigmapNN(nn types.NamespacedName) types.NamespacedName {
	return types.NamespacedName{
		Namespace: nn.Namespace,
		Name:      nn.Name + configmapResourceSuffix,
	}
}

func getCDCDeploymentNN(nn types.NamespacedName) types.NamespacedName {
	return types.NamespacedName{
		Namespace: nn.Namespace,
		Name:      nn.Name + controllerResourceSuffix,
	}
}
func getCDCrbacNN(nn types.NamespacedName) types.NamespacedName {
	return types.NamespacedName{
		Namespace: nn.Namespace,
		Name:      nn.Name + controllerResourceSuffix,
	}
}
func getJsonSchemaConfigmapNN(nn types.NamespacedName) types.NamespacedName {
	return types.NamespacedName{
		Namespace: nn.Namespace,
		Name:      nn.Name + "-jsonschema" + configmapResourceSuffix,
	}
}

func getServiceNN(nn types.NamespacedName) types.NamespacedName {
	return types.NamespacedName{
		Namespace: nn.Namespace,
		Name:      nn.Name + "-service",
	}
}

// ControllerDeploymentKey is the namespace/name of the composition-dynamic-controller Deployment
// Deploy creates for gvr in namespace.
func ControllerDeploymentKey(namespace string, gvr schema.GroupVersionResource) types.NamespacedName {
	return getCDCDeploymentNN(types.NamespacedName{
		Namespace: namespace,
		Name:      resourceNamer(gvr.Resource, gvr.Version),
	})
}

// drainPollInterval is how often waitForControllerDrained re-lists the retiring controller's pods.
// A migration is rare and operator-initiated, so a tight poll costs nothing and shortens the
// common case — a controller with nothing in flight exits in well under a second.
const drainPollInterval = 500 * time.Millisecond

// drainTimeoutMargin is added to the pod's own terminationGracePeriodSeconds to bound the wait.
// The kubelet SIGKILLs at the grace period, so a pod cannot outlive it; the margin only covers
// the kubelet noticing, the container exiting and the API reflecting the deletion.
const drainTimeoutMargin = 15 * time.Second

// defaultDrainGracePeriod is the wait bound when the rendered Deployment carries no explicit
// terminationGracePeriodSeconds, matching Kubernetes' own default for a pod that omits it.
const defaultDrainGracePeriod = 30 * time.Second

// waitForControllerDrained blocks until no pod of the just-deleted controller Deployment remains,
// so its ServiceAccount and RBAC can be deleted without cutting the identity out from under a
// reconcile that is still running (#263).
//
// It waits on POD ABSENCE rather than on the Deployment's own disappearance. The Deployment object
// is typically gone within milliseconds while its pod is still draining, so waiting for the
// Deployment would return immediately and prove nothing — the question is whether the PROCESS has
// exited, and only the pod answers that.
//
// Not implemented as a sleep. A sleep long enough to be safe (the full grace period) would be paid
// by every retirement including the overwhelming majority with nothing in flight, and a sleep short
// enough to tolerate would still race.
func waitForControllerDrained(ctx context.Context, kube client.Client, dep *appsv1.Deployment, timeout time.Duration) error {
	if kube == nil {
		return fmt.Errorf("no kube client to confirm the retiring controller has drained")
	}

	// The selector is the only thing that identifies the pods, so an empty one must FAIL rather
	// than match everything. client.MatchingLabels{} selects every pod in the namespace, which
	// would make the wait pass as soon as the namespace happened to be empty of pods and block
	// forever otherwise — in both directions an answer about the wrong question.
	if dep.Spec.Selector == nil || len(dep.Spec.Selector.MatchLabels) == 0 {
		return fmt.Errorf("rendered controller Deployment %s/%s has no label selector, so its pods "+
			"cannot be identified", dep.Namespace, dep.Name)
	}

	if timeout <= 0 {
		timeout = defaultDrainGracePeriod
		if g := dep.Spec.Template.Spec.TerminationGracePeriodSeconds; g != nil && *g > 0 {
			timeout = time.Duration(*g) * time.Second
		}
		timeout += drainTimeoutMargin
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		var pods corev1.PodList
		if err := kube.List(ctx, &pods,
			client.InNamespace(dep.Namespace),
			client.MatchingLabels(dep.Spec.Selector.MatchLabels),
		); err != nil {
			// A failed LIST is not evidence of absence. Returning nil here would be the
			// list-failure-as-zero mistake: the identity would be revoked because we could not see
			// the pod, which is precisely when we must not.
			return fmt.Errorf("listing pods of retiring controller %s/%s: %w", dep.Namespace, dep.Name, err)
		}
		if len(pods.Items) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			names := make([]string, 0, len(pods.Items))
			for i := range pods.Items {
				names = append(names, pods.Items[i].Name)
			}
			return fmt.Errorf("controller %s/%s still has %d pod(s) after %s: %s",
				dep.Namespace, dep.Name, len(pods.Items), timeout, strings.Join(names, ", "))
		case <-time.After(drainPollInterval):
		}
	}
}
