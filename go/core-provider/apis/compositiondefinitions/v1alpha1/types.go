package v1alpha1

import (
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
type CompositionDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []CompositionDefinition `json:"items"`
}

// GetItems of this CompositionDefinitionList.
func (l *CompositionDefinitionList) GetItems() []resource.Managed {
	items := make([]resource.Managed, len(l.Items))
	for i := range l.Items {
		items[i] = &l.Items[i]
	}
	return items
}

type Credentials struct {
	// Username: username for private repo
	Username string `json:"username"`
	// PasswordRef: reference to secret containing password for private repo.
	//
	// PRIVILEGE BOUNDARY, stated here because this is where someone writing a
	// CompositionDefinition will read it. The namespace is caller-supplied and is NOT confined to
	// the CompositionDefinition's own namespace: the referenced Secret is read with
	// core-provider's ServiceAccount, which can read Secrets cluster-wide, and nothing checks that
	// the author of this object was entitled to that Secret.
	//
	// So the credential named here travels to whatever `spec.chart.url` says. Granting create or
	// update on compositiondefinitions is therefore equivalent to granting cluster-wide Secret
	// READ, and should be treated as a privileged grant when writing RBAC — scope it the way you
	// would scope secrets access, not the way you would scope an application CR.
	//
	// THIS IS INTENDED AND WAS RULED ON, not an oversight (core-provider#306; Diego,
	// 2026-11-04). Cross-namespace references are a supported capability — deploy.go detects the
	// cross-namespace case and provisions a Role in the Secret's namespace so the fetch works, and
	// the equivalent surface in snowplow measured 23 of 26 references as cross-namespace. The
	// alternative considered was an RBAC check for a `use` verb against the requesting user, as
	// snowplow#1168 rules for its own endpoint Secrets. core-provider has no requesting user on a
	// reconcile: there is no SubjectAccessReview, UserInfo, impersonation or admission webhook
	// anywhere in this repo, so a reconcile sees this object and nothing about who wrote it. A
	// ServiceAccount substituted for the absent user would be a confused deputy with an
	// authorization check bolted on, which reads as authorised while being nothing of the kind.
	// The control therefore lives on who may write a CompositionDefinition at all.
	//
	// What is NOT in scope of that decision, and is fixed separately: the DESTINATION. A
	// credentialed fetch refuses link-local (including the cloud metadata service at
	// 169.254.169.254), unspecified and multicast addresses, checked at dial time on the resolved
	// address (plumbing dialguard, snowplow#1131).
	PasswordRef rtv1.SecretKeySelector `json:"passwordRef"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.version) || has(self.version)", message="Version is required once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.repo) || has(self.repo)", message="Repo is required once set"
type ChartInfo struct {
	// Url: oci or tgz full url
	Url string `json:"url"`
	// Version: desired chart version, needed for oci charts and for helm repo urls
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
	// Repo: Helm repo name
	// Should be set only for helm repo urls
	// If specified with OCI registries, it will be used as the repository name, instead the URL should contain the full path to the chart.
	// This is ignored for tgz archives
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=256
	Repo string `json:"repo,omitempty"`

	// InsecureSkipVerifyTLS: skip tls verification
	// +optional
	InsecureSkipVerifyTLS bool `json:"insecureSkipVerifyTLS,omitempty"`

	// Credentials: credentials for private repos
	// +optional
	Credentials *Credentials `json:"credentials,omitempty"`
}

type ChartInfoProps struct {
	// Url: oci or tgz full url
	Url string `json:"url"`
	// Version: desired chart version, needed for oci charts and for helm repo urls
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
	// Repo: helm repo name (for helm repo urls only)
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=256
	Repo string `json:"repo,omitempty"`

	// InsecureSkipVerifyTLS: skip tls verification
	// +optional
	InsecureSkipVerifyTLS bool `json:"insecureSkipVerifyTLS,omitempty"`

	// Credentials: credentials for private repos
	// +optional
	Credentials *Credentials `json:"credentials,omitempty"`
}

// DeploymentMode is the (status-only) classification of where the
// composition-dynamic-controller is deployed.
type DeploymentMode string

const (
	// DeploymentModeLocal: deployed into the management cluster (the default when no
	// target is referenced).
	DeploymentModeLocal DeploymentMode = "Local"
	// DeploymentModeRemote: deployed into a remote target cluster.
	DeploymentModeRemote DeploymentMode = "Remote"
)

// TargetReference references a namespaced KubernetesTarget by name. The target is always
// resolved in the referencing CompositionDefinition's OWN namespace, so no namespace field
// is carried here.
type TargetReference struct {
	// Name of the KubernetesTarget, resolved in the referencing object's own namespace.
	Name string `json:"name"`
}

// DeploymentTarget selects where the composition-dynamic-controller, the generated CRD
// and its RBAC are deployed. With no targetRef, deployment is local (the management
// cluster).
type DeploymentTarget struct {
	// TargetRef references a KubernetesTarget (resolved in this object's own namespace)
	// describing the remote cluster to deploy to. When omitted, deployment is local.
	// +optional
	TargetRef *TargetReference `json:"targetRef,omitempty"`
}

// Scale-to-zero is local-only in this release (#393, Diego 2026-11-11): a remote definition's controller
// and its instances live on the spoke, which needs spoke watches and spoke RBAC (core-provider#442).
// Rejected at admission so nobody believes the opt-in is active when it is not. (No quote characters in
// the rule: gofmt's doc-comment formatter rewrites two single quotes into a typographic quote.)
// +kubebuilder:validation:XValidation:rule="!(has(self.controller) && has(self.controller.scaleToZero) && self.controller.scaleToZero) || !(has(self.deploy) && has(self.deploy.targetRef) && size(self.deploy.targetRef.name) > 0)",message="spec.controller.scaleToZero is supported for local definitions only; for a remote spec.deploy.targetRef see core-provider#442"
// A pool is local-only and excludes scale-to-zero (docs/design/cdc-pooling.md §5c, Diego 2026-11-16).
// A remote definition's controller runs on its spoke, where a pool in the management namespace would
// span clusters; and a pool's process is shared, so one member scaling to zero would stop the others.
// +kubebuilder:validation:XValidation:rule="!(has(self.controller) && has(self.controller.pool)) || !(has(self.deploy) && has(self.deploy.targetRef) && size(self.deploy.targetRef.name) > 0)",message="spec.controller.pool is supported for local definitions only (docs/design/cdc-pooling.md §5c)"
// +kubebuilder:validation:XValidation:rule="!(has(self.controller) && has(self.controller.pool) && has(self.controller.scaleToZero) && self.controller.scaleToZero)",message="spec.controller.pool and spec.controller.scaleToZero are mutually exclusive: a pool's process is shared by every member"
type CompositionDefinitionSpec struct {
	// rtv1.ManagedSpec `json:",inline"`
	Chart *ChartInfo `json:"chart,omitempty"`

	// Deploy: selects whether the composition-dynamic-controller (and the generated
	// CRD and RBAC) are deployed locally in the management cluster (the default) or to
	// a remote target cluster referenced by a KubernetesTarget. When omitted, deployment
	// is local.
	// +optional
	Deploy *DeploymentTarget `json:"deploy,omitempty"`

	// ApiRef references a RESTAction whose calls are resolved (via snowplow, under an
	// authn-issued scoped service identity) each reconcile; results are keyed by call name
	// under ".api" in the projection root. Shape copied from snowplow's spec.apiRef
	// (name/namespace + inline extras).
	// +optional
	ApiRef *ApiReference `json:"apiRef,omitempty"`

	// StatusDataTemplate declares extra fields projected onto the generated Composition
	// CRD's status and populated by the controller each reconcile (snowplow
	// widgetDataTemplate shape). Each entry's ${ jq } Expression is evaluated over the
	// combined source root (built-in self/spec/status, helm, and api) and written under
	// .status at ForPath.
	// +optional
	// +listType=map
	// +listMapKey=forPath
	StatusDataTemplate []StatusFieldMapping `json:"statusDataTemplate,omitempty"`

	// UpgradePolicy governs whether existing composition instances are migrated onto a newly
	// served chart version:
	//   - Automatic (default): eagerly self-heal every owned instance onto the current version
	//     (retire the old version's controller once nothing references it). Backward-compatible
	//     with the pre-policy behavior.
	//   - Manual: keep existing instances on their current version (the new version is still served,
	//     so coexistence holds); migrate only when the krateo.io/upgrade-to-version annotation names
	//     the current served version.
	//   - Paused: never migrate; freeze existing instances on their version.
	// Under Manual/Paused the old version's controller is kept so coexisting instances are not
	// orphaned; served-version pruning still keeps any version an instance's label references.
	// +optional
	// +kubebuilder:validation:Enum=Automatic;Manual;Paused
	// +kubebuilder:default=Automatic
	UpgradePolicy UpgradePolicy `json:"upgradePolicy,omitempty"`

	// Controller tunes the composition-dynamic-controller (CDC) Deployment for THIS definition's
	// Kind. It is the per-Kind escape hatch from the otherwise-uniform, cluster-wide CDC template:
	// a Kind with many instances can raise reconcile concurrency and lengthen its resync without
	// touching — or restarting — any other controller. Omitted fields fall back to the chart-global
	// defaults, so existing definitions render byte-identically.
	// +optional
	Controller *ControllerConfig `json:"controller,omitempty"`
}

// ControllerConfig carries per-CompositionDefinition tuning for the composition-dynamic-controller.
// Every field is optional and, when unset, inherits the chart-global default baked into the CDC
// Deployment template. Note: replicas is deliberately NOT exposed here — running more than one CDC
// replica for a Kind is unsafe until sharding or leader election exists (two replicas both reconcile
// every instance and double-drive the same Helm release). Use Workers for in-process concurrency.
type ControllerConfig struct {
	// Workers is the number of concurrent reconcile workers inside the CDC process (the CDC's
	// -workers flag; one shared work queue feeds them). It is the primary lever for reconcile
	// throughput on a Kind with many instances. Defaults to the chart-global value (1) when unset.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	Workers *int32 `json:"workers,omitempty"`

	// ResyncInterval is how often the CDC re-lists and re-observes every instance of the Kind to
	// correct drift (the CDC's -resync-interval flag). On a Kind with many instances a short resync
	// makes the controller re-enqueue the whole population continuously; lengthening it lets steady
	// state ride watch events instead of full sweeps. Defaults to the chart-global value (3m) when
	// unset. A Go duration string (e.g. "30m", "1h"); it is passed verbatim to the CDC's
	// -resync-interval flag. Typed as string (not metav1.Duration) deliberately: core-provider runs
	// no admission webhook, so the CRD schema is the only gate, and a plain string always decodes —
	// a bad value can never fail the typed informer's atomic list-decode and stall every
	// CompositionDefinition. The Pattern + CEL rules then reject unit-less/garbage values and
	// non-positive durations ("0s"/negative would crash the CDC's ticker) at admission.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="resyncInterval must be a positive Go duration such as 30m or 1h"
	ResyncInterval *string `json:"resyncInterval,omitempty"`

	// Pool names a shared controller pool this Kind's controller runs in (docs/design/cdc-pooling.md §5c).
	// Absent, the definition has its own dedicated controller, exactly as before. Set, its controller is
	// a member of the pool cdc-pool-<pool> in the management namespace, which holds the union of its
	// members' permissions; names let Kinds with similar permissions share one. Local definitions only,
	// and not with scaleToZero. At most 54 characters, so the pool's object names (cdc-pool-<pool>) stay
	// valid DNS-1123 labels.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=54
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Pool *string `json:"pool,omitempty"`

	// ScaleToZero runs this Kind's controller at zero replicas while no instance of it exists, and
	// wakes it when the first one appears (#393). Off when unset: the controller runs at the chart's
	// replica count, as before. Opting in costs one metadata watch on this Kind's instances, which is
	// what wakes it, so it is per definition rather than global. While idle the definition reports
	// Ready=True and ControllerActive=False, both with reason ScaledToZero (plumbing conditions, the
	// strings shared with oasgen-provider).
	// +optional
	ScaleToZero *bool `json:"scaleToZero,omitempty"`

	// ScaleToZeroHoldDown is how long the Kind must have no instances before its controller is scaled
	// to zero, so a delete followed by a re-create does not bounce it. Defaults to 10m. A Go duration
	// string, typed as a string for the same reason as ResyncInterval.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="scaleToZeroHoldDown must be a positive Go duration"
	ScaleToZeroHoldDown *string `json:"scaleToZeroHoldDown,omitempty"`

	// Resources sets the CDC container's compute resource requests/limits for this Kind. The shared
	// template ships resources:{} (no requests), which is bad practice and makes the CDC
	// unschedulable-aware and impossible to autoscale; set requests here for a Kind whose CDC needs
	// a memory/CPU floor (e.g. one caching many instances). When unset, the chart-global default
	// (cdc.resources) applies, so existing definitions render byte-identically.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// UpgradePolicy selects the composition-instance migration strategy on a chart-version bump.
type UpgradePolicy string

const (
	// UpgradePolicyAutomatic eagerly migrates every owned instance onto the current version.
	UpgradePolicyAutomatic UpgradePolicy = "Automatic"
	// UpgradePolicyManual migrates only when the upgrade-to-version annotation approves it.
	UpgradePolicyManual UpgradePolicy = "Manual"
	// UpgradePolicyPaused freezes migration entirely.
	UpgradePolicyPaused UpgradePolicy = "Paused"

	// AnnotationKeyUpgradeToVersion, on a CompositionDefinition with UpgradePolicy=Manual, approves
	// migrating its instances onto the named served version (typically the current one).
	AnnotationKeyUpgradeToVersion = "krateo.io/upgrade-to-version"
)

// MigrationApproved reports whether the controller should migrate existing instances onto
// targetVersion given the definition's UpgradePolicy (and, for Manual, the upgrade-to-version
// annotation). An empty policy is treated as Automatic for backward compatibility.
func (cd *CompositionDefinition) MigrationApproved(targetVersion string) bool {
	switch cd.Spec.UpgradePolicy {
	case UpgradePolicyPaused:
		return false
	case UpgradePolicyManual:
		return cd.GetAnnotations()[AnnotationKeyUpgradeToVersion] == targetVersion
	default: // Automatic or unset
		return true
	}
}

// ApiReference references a RESTAction to resolve for status projection. It mirrors
// snowplow's spec.apiRef: a name/namespace plus an inline, free-form extras map.
type ApiReference struct {
	// Name of the referenced RESTAction.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the referenced RESTAction.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Extras are author-declared STATIC values merged into the RESTAction's jq root
	// (snowplow spec.apiRef.extras). Input-only — they never surface in status. The
	// controller additionally injects per-instance context (compositionId, namespace, …)
	// which merges over these (request-wins).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Extras *apiextensionsv1.JSON `json:"extras,omitempty"`

	// Delegation lets this definition's composition controller act on behalf of a caller through
	// authn's POST /delegate (#405). It is rendered onto the controller's authn ServiceAccount mapping
	// (spec.delegation) on every apply; a value set by hand on the mapping is replaced. Absent means no
	// delegation key on the mapping at all, which authn answers with 403 DelegationNotAllowed.
	//
	// The bounds are authn's own (serviceaccounts.serviceaccount.authn.krateo.io): if this field
	// accepted more, the apiserver would reject the mapping's update instead, far from this object.
	// Enabling it on a real definition is a platform decision, not a default.
	// +optional
	Delegation *ApiRefDelegation `json:"delegation,omitempty"`
}

// ApiRefDelegation mirrors authn's ServiceAccount mapping spec.delegation, with its bounds.
type ApiRefDelegation struct {
	// Allowed enables delegation for the controller's identity. Required whenever delegation is set.
	Allowed bool `json:"allowed"`

	// MaxDepth limits how deep a delegation chain may go. Unset leaves authn's own default (3, applied
	// in authn's code, not by its CRD), so it is rendered only when set.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxDepth *int32 `json:"maxDepth,omitempty"`
}

// StatusFieldMapping is one projected status field (snowplow widgetDataTemplate item).
type StatusFieldMapping struct {
	// ForPath is the dotted path under .status to write, e.g. "endpoint" or "network.host".
	// +kubebuilder:validation:MinLength=1
	ForPath string `json:"forPath"`

	// Expression is a ${ jq } program evaluated over the projection root; a bare path is
	// the trivial copy. Plain literals (no ${ } wrapper) are written verbatim.
	// +kubebuilder:validation:MinLength=1
	Expression string `json:"expression"`

	// Type optionally pins the generated status property's scalar JSON-schema type. For
	// complex (object/array) outputs use Schema or PreserveUnknownFields instead.
	// +optional
	// +kubebuilder:validation:Enum=string;integer;number;boolean;object;array
	Type string `json:"type,omitempty"`

	// Schema optionally supplies the full JSON-schema for a complex output (object, array,
	// array of objects). Mutually exclusive with PreserveUnknownFields.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Schema *apiextensionsv1.JSONSchemaProps `json:"schema,omitempty"`

	// PreserveUnknownFields generates the status node with
	// x-kubernetes-preserve-unknown-fields: true, retaining an arbitrary structure without
	// pruning. Mutually exclusive with Type/Schema.
	// +optional
	PreserveUnknownFields bool `json:"preserveUnknownFields,omitempty"`
}

// KubernetesTargetSpec describes how to reach a remote target cluster.
type KubernetesTargetSpec struct {
	// KubeconfigRef: reference to a Kubernetes Secret key holding the kubeconfig of the
	// target cluster. The Secret is the credential rotation seam - populate and rotate
	// it via External Secrets Operator.
	KubeconfigRef rtv1.SecretKeySelector `json:"kubeconfigRef"`

	// AuthnURL is the base URL of the authn service INSIDE the target cluster, which a composition
	// controller running there calls to exchange its projected ServiceAccount token for a JWT
	// (definitions with an apiRef). That token is minted by the target's apiserver, so only an authn
	// in the same target can validate it. Optional: when empty, the controller is given
	// http://authn.<authn namespace>.svc.cluster.local:8082, where the authn namespace is the one
	// core-provider writes the controller's authn mapping into (COMPOSITION_AUTHN_NAMESPACE).
	// Set it when the target's authn Service has another name, namespace or port (#251).
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://[^\s]+$`
	AuthnURL string `json:"authnUrl,omitempty"`
}

// KubernetesTargetStatus reports the observed reachability of the target cluster, refreshed by the
// KubernetesTarget reachability reconciler. It turns the passive reference into a registered spoke
// with lifecycle: a Ready condition, the reported Kubernetes version, and the credential the last
// probe used (for rotation traceability).
type KubernetesTargetStatus struct {
	rtv1.ConditionedStatus `json:",inline"`

	// ConnectionStatus is Healthy when the target cluster's API is reachable, Down otherwise.
	// +optional
	ConnectionStatus string `json:"connectionStatus,omitempty"`

	// Version is the Kubernetes version reported by the target cluster on the last successful probe.
	// +optional
	Version string `json:"version,omitempty"`

	// KubeconfigSecretResourceVersion is the resourceVersion of the kubeconfig Secret used by the
	// last successful probe, for rotation traceability.
	// +optional
	KubeconfigSecretResourceVersion string `json:"kubeconfigSecretResourceVersion,omitempty"`

	// LastProbeTime is when the target's reachability was last checked.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced,categories={krateo,core}
//+kubebuilder:printcolumn:name="CONNECTION",type="string",JSONPath=".status.connectionStatus"
//+kubebuilder:printcolumn:name="VERSION",type="string",JSONPath=".status.version"
//+kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// KubernetesTarget is a namespaced reference to a remote cluster, used by a
// CompositionDefinition's spec.deploy.targetRef (resolved in the CompositionDefinition's
// own namespace) to deploy compositions remotely. It is namespaced so per-namespace
// writes through snowplow /call (which force-namespaces every write) can create it.
type KubernetesTarget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec KubernetesTargetSpec `json:"spec,omitempty"`
	// +optional
	Status KubernetesTargetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KubernetesTargetList is a list of KubernetesTarget.
type KubernetesTargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []KubernetesTarget `json:"items"`
}

type VersionDetail struct {
	// Version: the version of the chart that is served. It is the version of the CRD.
	// +optional
	Version string `json:"version"`

	// Served: whether the version is served
	// +optional
	Served bool `json:"served"`

	// Stored: whether the version is stored
	// +optional
	Stored bool `json:"stored"`

	// Chart: the chart information
	// +optional
	Chart *ChartInfoProps `json:"chart"`
}

type Managed struct {
	// VersionInfo: the version information of the chart
	// +optional
	VersionInfo []VersionDetail `json:"versionInfo,omitempty"`

	// Group: the generated custom resource Group
	// +optional
	Group string `json:"group,omitempty"`

	// Kind: the generated custom resource Kind
	// +optional
	Kind string `json:"kind,omitempty"`
}

// ControllerMode says whether a definition's controller runs in its own Deployment or in a pool.
// +kubebuilder:validation:Enum=Dedicated;Pool
type ControllerMode string

const (
	// ControllerModeDedicated: the Kind x version's own Deployment, <resource>-<version>-controller.
	ControllerModeDedicated ControllerMode = "Dedicated"
	// ControllerModePool: a member of pool spec.controller.pool, run by cdc-pool-<pool>
	// (docs/design/cdc-pooling.md section 5c).
	ControllerModePool ControllerMode = "Pool"
)

// ControllerStatus names the Deployment that runs this definition's controller (core-provider#580):
// the supported way to find it, instead of deriving <resource>-<version>-controller, which a pooled
// Kind does not have. A follower (another definition owns its Kind x version) names its owner's
// controller, which is the one that reconciles its instances. Recorded when the definition's
// deployment is observed up to date, so it describes what is deployed, not what the spec asks for;
// Ready and ControllerActive say whether that Deployment is running.
type ControllerStatus struct {
	// Mode: Dedicated or Pool.
	Mode ControllerMode `json:"mode"`

	// Namespace: where the Deployment is, on the cluster status.target names.
	Namespace string `json:"namespace"`

	// Deployment: the name of the Deployment that runs the controller.
	Deployment string `json:"deployment"`

	// Pool: the pool's name, for mode Pool.
	// +optional
	Pool string `json:"pool,omitempty"`
}

// TargetStatus reports the cluster the composition-dynamic-controller (and the
// generated CRD and RBAC) are deployed to.
type TargetStatus struct {
	// Mode: where the controller is deployed (Local or Remote).
	// +optional
	Mode string `json:"mode,omitempty"`

	// ConnectionStatus: Healthy when the target cluster is reachable, Down otherwise.
	// +optional
	ConnectionStatus string `json:"connectionStatus,omitempty"`

	// Version: the Kubernetes version reported by the target cluster.
	// +optional
	Version string `json:"version,omitempty"`

	// KubeconfigSecretResourceVersion: the resourceVersion of the kubeconfig Secret last
	// used to reach a remote target, for credential-rotation traceability.
	// +optional
	KubeconfigSecretResourceVersion string `json:"kubeconfigSecretResourceVersion,omitempty"`
}

// DefinitionReference names a CompositionDefinition (core-provider#616).
type DefinitionReference struct {
	// Namespace: the referenced definition's namespace.
	Namespace string `json:"namespace"`

	// Name: the referenced definition's name.
	Name string `json:"name"`
}

// MigratedValuesMaxInstances bounds MigratedValues.Refused and MigratedValues.Unchecked, and the
// instances the MigratedValuesValid message names. The CRD's maxItems on both lists must equal it.
const MigratedValuesMaxInstances = 10

// MigratedValuesMaxFields bounds MigratedValuesRefusedInstance.Fields, and the fields the
// MigratedValuesValid message names per instance. The CRD's maxItems on Fields must equal it.
const MigratedValuesMaxFields = 5

// MigratedValues is the structured form of the MigratedValuesValid condition (core-provider#623).
type MigratedValues struct {
	// Version: the served version the instances were checked against.
	Version string `json:"version"`

	// RefusedCount: how many instances that version refuses. Not capped: it can exceed len(refused).
	// +kubebuilder:validation:Minimum=0
	RefusedCount int `json:"refusedCount"`

	// UncheckedCount: how many instances could not be judged (the dry run failed for a reason that is
	// not a spec validation error). Not capped: it can exceed len(unchecked).
	// +kubebuilder:validation:Minimum=0
	UncheckedCount int `json:"uncheckedCount"`

	// Refused: the first refused instances, at most MigratedValuesMaxInstances, in the order the
	// condition's message names them.
	// +kubebuilder:validation:MaxItems=10
	// +optional
	Refused []MigratedValuesRefusedInstance `json:"refused,omitempty"`

	// Unchecked: the first instances that could not be judged, at most MigratedValuesMaxInstances, in
	// the order the condition's message names them.
	// +kubebuilder:validation:MaxItems=10
	// +optional
	Unchecked []MigratedValuesInstance `json:"unchecked,omitempty"`
}

// MigratedValuesInstance names a composition instance.
type MigratedValuesInstance struct {
	// Namespace: the instance's namespace.
	Namespace string `json:"namespace"`

	// Name: the instance's name.
	Name string `json:"name"`
}

// MigratedValuesRefusedInstance is an instance the served version refuses, with the spec fields it
// names.
type MigratedValuesRefusedInstance struct {
	// Namespace: the instance's namespace.
	Namespace string `json:"namespace"`

	// Name: the instance's name.
	Name string `json:"name"`

	// Fields: the spec field paths the apiserver refuses, sorted, at most MigratedValuesMaxFields.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=5
	Fields []string `json:"fields"`

	// FieldCount: how many fields the apiserver refuses. Not capped: it can exceed len(fields).
	// +kubebuilder:validation:Minimum=1
	FieldCount int `json:"fieldCount"`
}

// CompositionDefinitionStatus is the status of a CompositionDefinition.
type CompositionDefinitionStatus struct {
	rtv1.ConditionedStatus `json:",inline"`

	// ObservedGeneration: the metadata.generation the Ready condition beside it was determined from
	// (core-provider#644). Lower than metadata.generation, Ready is about an EARLIER spec: the definition
	// has not been reconciled since the spec changed, the pass failed before it could determine Ready, it
	// is paused or its update is withheld by policy, or an update is in progress and Ready is kept from
	// before it. Equal to it with Ready=True, the spec is applied: status.apiVersion is the version its
	// chart yields. ABSENT says nothing: the writer does not report it (an older core-provider), or no
	// pass has determined Ready yet. It does not advance once the definition is being deleted.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Target: information about the cluster the controller is deployed to.
	// +optional
	Target *TargetStatus `json:"target,omitempty"`

	// Controller: the Deployment that runs this definition's controller (core-provider#580).
	// +optional
	Controller *ControllerStatus `json:"controller,omitempty"`

	// Owner: the definition that owns this one's Kind x version (core-provider#616). Set only on a
	// follower: another definition targets the same Kind and version, generated its CRD and runs the
	// controller that reconciles this definition's instances. Absent on the owner itself and on a
	// definition with no follower relation, and cleared when this definition becomes the owner.
	// Recorded each time the definition is observed, from the same election that names the owner in
	// its conditions' messages. It describes the last successful observation: a definition being
	// deleted, or one whose observation stopped on an error before the election, keeps the value it
	// last recorded until its next successful observation.
	// +optional
	Owner *DefinitionReference `json:"owner,omitempty"`

	// MigratedValues: the instances a version migration left holding values the served version
	// refuses, as structured data (core-provider#623). It is built from the same check, in the same
	// call, as the MigratedValuesValid condition, so the two always agree: the instances named here are
	// the ones its message names, in the same order. Absent while that condition is absent (no
	// migration has moved an instance yet) and when the instance list itself could not be read (the
	// condition is Unknown and says why); cleared when the condition becomes True.
	// +optional
	MigratedValues *MigratedValues `json:"migratedValues,omitempty"`

	// Kind: the kind of the custom resource - Last applied kind
	Kind string `json:"kind,omitempty"`

	// Resource: the resource of the custom resource - Last applied resource
	Resource string `json:"resource,omitempty"`

	// ApiVersion: the api version of the custom resource - Last applied apiVersion
	ApiVersion string `json:"apiVersion,omitempty"`

	// Managed: information about the managed resources
	Managed Managed `json:"managed,omitempty"`

	// PackageURL: .tgz or oci chart direct url
	// +optional
	PackageURL string `json:"packageUrl,omitempty"`

	// Digest: the digest of the managed resources
	// +optional
	Digest string `json:"digest,omitempty"`

	// Contents is what the chart renders at its DEFAULT values: the kinds, and how many objects of
	// each, of the release manifest helm would install (core-provider#599). It is for display (the
	// portal's blueprint page); it never affects Ready. A composition's own values can change the
	// counts, which is why Values is always "default".
	// +optional
	Contents *ChartContents `json:"contents,omitempty"`

	// Chart is what core-provider read from the chart's own Chart.yaml when it read the chart for
	// Contents (core-provider#671): who maintains the blueprint, and the chart version that was read.
	// Written on the same pass and from the same loaded chart as Contents, never from a second fetch
	// and never fabricated. ABSENT: this core-provider could not read the chart's Chart.yaml, or it is
	// older than this field; a reader treats absent as "chart unread", not as "no maintainers".
	// +optional
	Chart *ChartStatus `json:"chart,omitempty"`

	// SchemaDefaults is the machine-readable list behind the SchemaDefaultsMatchValues condition
	// (core-provider#686): where the chart's values.schema.json declares a default that its values.yaml
	// disagrees with. The condition's MESSAGE is human text and is not a contract; read this field
	// instead. Set in the same pass, from the same computed differences and in the same order as the
	// message, so the two cannot disagree. ABSENT when the condition is Aligned, when it is
	// OwnedByAnotherDefinition (the owner's status carries it), and from a core-provider older than this
	// field: absent is never "differences unknown" and never an empty object.
	// +optional
	SchemaDefaults *SchemaDefaultsStatus `json:"schemaDefaults,omitempty"`

	// ScaleToZero is the scale-to-zero reconciler's state for a definition that opted in (#393).
	// Absent otherwise. Written ONLY by that reconciler, with a merge patch on this field; the
	// definition's main reconciler reads it to report Ready and ControllerActive.
	// +optional
	ScaleToZero *ScaleToZeroStatus `json:"scaleToZero,omitempty"`

	// ApiRefRBAC says where this definition's apiRef read grants are, and which stay cluster-wide
	// (core-provider#691). Set while the definition declares an apiRef on a local cluster, in the same
	// pass that sets ApiRefRBACComplete; read it instead of that condition's message. Absent without an
	// apiRef, and from a core-provider older than this field.
	// +optional
	ApiRefRBAC *ApiRefRBACStatus `json:"apiRefRBAC,omitempty"`

	// ServedVersionsPendingPrune records, per served version of this definition's CRD that has
	// become prune-eligible (no definition references it, no instance is labelled with it, no stored
	// helm revision names it), when it first became so (#454). A version is pruned only after it has
	// stayed eligible for the grace window, so a render that read the CRD before the prune never
	// looks the version up after it. Each version has its own clock: a batch released at once by a
	// helm history rotation is N independent windows. A version that stops being eligible is
	// dropped from the list. Written by the definition's own reconciler, in Observe.
	// +optional
	ServedVersionsPendingPrune []PendingPrune `json:"servedVersionsPendingPrune,omitempty"`

	// ServedVersionsHeld lists, per served version of this definition's CRD that is NOT prune-eligible
	// (other than the current version and the storage version, which are never pruned), why it is
	// held: another definition references it, instances carry its label, or a helm revision that helm
	// will read names it (#471). Without it a held version was visible only in a log line, and
	// servedVersionsPendingPrune read empty while a version stayed served indefinitely. Recomputed on
	// every Observe; empty when nothing is held. Written by the definition's own reconciler.
	// +optional
	ServedVersionsHeld []HeldVersion `json:"servedVersionsHeld,omitempty"`

	// WrittenBy: the binary that made this definition's last status write (core-provider#669). Its
	// name is "core-provider", whose conditions are listed in docs/cd-ready-reasons.json, and its
	// version is the core-provider repository tag to read that table at. It names the LAST status
	// write only: a condition that write did not rewrite (a type a newer version no longer writes) can
	// still carry an earlier binary's text. Set on the object the pass is about to write anyway, never
	// a reason for a write, so a pass that changes nothing else writes nothing; a pass that ends before
	// core-provider's own code runs (a paused definition) leaves it as it was. ABSENT: the last status
	// write came from a core-provider older than this field.
	// +optional
	WrittenBy *WrittenBy `json:"writtenBy,omitempty"`
}

// ChartStatus is what was read from a chart's Chart.yaml (core-provider#671).
type ChartStatus struct {
	// Version is the chart version the maintainers were read from (Chart.yaml version), so a
	// consumer can say "owner as of chart X" when it differs from spec.chart.version. Written even
	// when the chart declares no maintainers: it states what was read.
	Version string `json:"version"`

	// Maintainers is Chart.yaml's maintainers list, exactly helm's shape and nothing added. ABSENT
	// (never an empty list) when the chart declares none, so "no maintainers" and "an older
	// core-provider" both read as absent: the owner is unknown. A maintainer with no name is not
	// listed (helm's own shape requires one).
	// +optional
	// +listType=atomic
	Maintainers []ChartMaintainer `json:"maintainers,omitempty"`
}

// ChartMaintainer is a Chart.yaml maintainer reduced to name and url. helm's chart.Maintainer also
// has an email; it is intentionally not copied: cluster status is dumped wholesale into snapshots,
// telemetry and support bundles, so PII-shaped data does not enter it. The contact is in the chart.
type ChartMaintainer struct {
	// Name is the maintainer's name. Required, as in helm.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// URL is the maintainer's URL. Omitted when the chart gives none.
	// +optional
	URL string `json:"url,omitempty"`
}

// WrittenBy names a binary by its build stamp (core-provider#669).
type WrittenBy struct {
	// Name is the binary: "core-provider" on a CompositionDefinition.
	Name string `json:"name"`
	// Version is the binary's build version: the bare core-provider repository tag on a release
	// build (e.g. 2.13.135), "dev" on a build that was not stamped. Written as the build carries it.
	Version string `json:"version"`
	// Commit is the commit the binary was built from, "unknown" on a build that was not stamped.
	Commit string `json:"commit"`
}

// HeldVersion is one served version the prune keeps, with the reason (#471).
type HeldVersion struct {
	// Version is the served version, e.g. v0-3-49; "*" when every version is held because an instance
	// carries no usable version label (the prune then refuses to remove anything).
	Version string `json:"version"`
	// Reason is the prune's keep reason, as logged in "served-version prune evaluation", e.g.
	// "release-revisions=1:[openstack/umbrella(rev 4, ...)]", "instances=3", "referenced".
	Reason string `json:"reason"`
}

// PendingPrune is one served version waiting out the prune grace window (#454).
type PendingPrune struct {
	// Version is the served version, e.g. v0-1-42.
	Version string `json:"version"`
	// EligibleSince is when this reconciler first saw the version prune-eligible.
	EligibleSince metav1.Time `json:"eligibleSince"`
}

// ScaleToZeroStatus records what provider-runtime's scaletozero helper last decided for this
// definition's controller.
type ScaleToZeroStatus struct {
	// LastSeen is when an instance of the Kind was last seen; the hold-down counts from it. It is the
	// helper's Result.LastSeen, persisted because the helper keeps no state.
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`

	// Replicas is the controller Deployment's replicas after the last decision: 0 when idle.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Reason is the helper's reason for that decision: Active, IdleHoldDown, Idle or Unknown.
	// +optional
	Reason string `json:"reason,omitempty"`

	// HeldBy names the live definitions that share this controller and have NOT opted in. While it is
	// non-empty the shared controller is kept running (Diego, 2026-11-11: every sharing definition
	// must opt in).
	// +optional
	HeldBy []string `json:"heldBy,omitempty"`
}

// ApiRefRBACStatus is where a definition's apiRef reads are granted. A read that a composition's
// extras place in a namespace is granted by a Role in that namespace; only a read whose namespace
// no composition can resolve (it comes from a prior stage's output) is cluster-wide.
type ApiRefRBACStatus struct {
	// Namespaces are the namespaces holding a Role and RoleBinding for this definition's apiRef reads
	// (one per namespace a composition's reads land in), sorted.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// ClusterWide are the reads kept in the definition's ClusterRole because their namespace is
	// resolved from a prior stage's output, so no composition can place them. Sorted. Cluster-scoped
	// resources are not listed: they are cluster-wide by nature.
	// +optional
	ClusterWide []ApiRefClusterWideRow `json:"clusterWide,omitempty"`

	// UnresolvedCompositions are the compositions whose apiRef request snowplow refused (reason
	// ExtrasTooLarge: HTTP 431, or InvalidApiRef: 400), sorted by namespace/name and capped at 20. Their
	// reads are not granted by this pass and their existing grants are kept: the Roles are only added to
	// while any is listed. One composition never fails the definition.
	// +optional
	UnresolvedCompositions []ApiRefUnresolvedComposition `json:"unresolvedCompositions,omitempty"`
}

// ApiRefUnresolvedComposition is one composition whose reads could not be resolved.
type ApiRefUnresolvedComposition struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// ApiRefClusterWideRow is one namespaced read granted cluster-wide.
type ApiRefClusterWideRow struct {
	Group    string `json:"group"`
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced,categories={krateo,defs,core}
//+kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
//+kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
//+kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
//+kubebuilder:printcolumn:name="API VERSION",type="string",JSONPath=".status.apiVersion",priority=10
//+kubebuilder:printcolumn:name="KIND",type="string",JSONPath=".status.kind",priority=10
//+kubebuilder:printcolumn:name="PACKAGE URL",type="string",JSONPath=".status.packageUrl",priority=10
//+kubebuilder:printcolumn:name="TARGET",type="string",JSONPath=".status.target.mode",priority=10
//+kubebuilder:printcolumn:name="CONNECTION",type="string",JSONPath=".status.target.connectionStatus",priority=10
//+kubebuilder:printcolumn:name="GENERATION",type="integer",JSONPath=".metadata.generation",priority=10
//+kubebuilder:printcolumn:name="OBSERVED",type="integer",JSONPath=".status.observedGeneration",priority=10

// CompositionDefinition is a definition type with a spec and a status.
type CompositionDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CompositionDefinitionSpec   `json:"spec,omitempty"`
	Status CompositionDefinitionStatus `json:"status,omitempty"`
}

// SetConditions of this CompositionDefinition.
func (mg *CompositionDefinition) SetConditions(c ...rtv1.Condition) {
	mg.Status.SetConditions(c...)
}

// GetCondition of this CompositionDefinition.
func (mg *CompositionDefinition) GetCondition(ct rtv1.ConditionType) rtv1.Condition {
	return mg.Status.GetCondition(ct)
}

// ChartContentsMaxKinds bounds ChartContents.Kinds; the kinds beyond it are counted in Omitted.
const ChartContentsMaxKinds = 50

// ChartContentsUnknownMaxLength bounds ChartContents.Unknown, in bytes (the CRD MaxLength above).
const ChartContentsUnknownMaxLength = 1024

// ChartContents is what a definition's chart renders at its default values (core-provider#599).
//
// It is the release MANIFEST, as `helm get manifest` would show it after an install at default
// values: rendered templates only. Hooks are excluded (helm keeps them out of the manifest and
// deletes most of them after they run), and so are the files in crds/ (helm installs them apart
// from the release and never tracks them). Objects rendered by subcharts are included.
//
// Exactly one of Kinds and Unknown is set. Unknown names why the chart could not be rendered at its
// defaults (a value the chart requires, a values.schema.json the defaults violate, a missing
// dependency), so an empty Kinds is never read as "creates nothing".
type ChartContents struct {
	// ChartVersion is the version of the chart this was rendered from. After a chart version
	// change it still names the previous version until the new render is written.
	ChartVersion string `json:"chartVersion"`

	// Values is always "default": the chart's own values, no composition's.
	// +kubebuilder:validation:Enum=default
	Values string `json:"values"`

	// Kinds lists each kind the release manifest contains with its object count, by count
	// descending, then apiVersion, then kind. At most ChartContentsMaxKinds entries.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +listType=atomic
	Kinds []ChartContentsKind `json:"kinds,omitempty"`

	// Omitted is the number of kinds not listed because Kinds is capped. ABSENT (omitempty) when the
	// list is complete, never a literal 0: a reader treats absent as 0.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Omitted int `json:"omitted,omitempty"`

	// Unknown, when set, says why the chart could not be rendered at its default values; Kinds is
	// then empty. It is helm's error from an OFFLINE render (no cluster client), so it carries no
	// apiserver text; its content comes from the chart's own files and default values. At most
	// ChartContentsUnknownMaxLength bytes, truncated with a " ... (truncated)" mark.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Unknown string `json:"unknown,omitempty"`
}

// ChartContentsKind is one kind of the rendered release manifest and how many objects of it.
//
// The length bounds are ENFORCED HERE, at admission, not only by the writer (installer, reviewing
// #605: "a bound that lives in the writer is a bound a different writer does not have"). They are the
// Kubernetes name limits, generous on purpose: a cap tighter than a valid name would make the
// definition's status write fail. apiVersion: a DNS-1123 subdomain group (253) + "/" + a DNS-1123
// label version (63) = 317. kind: 253, an upper bound no valid kind exceeds.
type ChartContentsKind struct {
	// +kubebuilder:validation:MaxLength=317
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MaxLength=253
	Kind string `json:"kind"`
	// +kubebuilder:validation:Minimum=1
	Count int `json:"count"`
}

// SchemaDefaultKind says how one default of a chart's values.schema.json relates to its values.yaml.
type SchemaDefaultKind string

const (
	// SchemaDefaultValueDiffers: the schema declares a default and values.yaml has a different value
	// at the path. The generated CRD defaults to values.yaml's value.
	SchemaDefaultValueDiffers SchemaDefaultKind = "valueDiffers"
	// SchemaDefaultWithoutValuesKey: the schema declares a default and values.yaml has no key (or a
	// null) at the path, so the generated CRD has no default there; helm applies none either.
	SchemaDefaultWithoutValuesKey SchemaDefaultKind = "schemaDefaultWithoutValuesKey"
	// SchemaDefaultValuesInvalid: values.yaml has a value at the path that the schema's own
	// constraints refuse, so it was not made a CRD default.
	SchemaDefaultValuesInvalid SchemaDefaultKind = "valuesValueInvalid"
)

// SchemaDefaultsStatus lists the differences between a chart's values.schema.json defaults and its
// values.yaml (core-provider#686), at most 10 of them, sorted by path.
type SchemaDefaultsStatus struct {
	// Differ is the listed differences, sorted by path, in the order the condition message names them.
	// At most 10; Dropped counts the rest.
	// +listType=atomic
	Differ []SchemaDefaultDifference `json:"differ"`

	// Dropped is how many more differences exist beyond those listed in Differ; 0 when all are listed.
	Dropped int `json:"dropped"`
}

// SchemaDefaultDifference is one path where the chart's values.schema.json and values.yaml disagree.
// Schema and Values are JSON values (apiextensionsv1.JSON: a boolean, number, string, array or
// object exactly as the chart wrote it, not a string holding JSON), which round-trips through the CRD
// as an untyped field.
type SchemaDefaultDifference struct {
	// Path is the dotted values path ("restdefinitions.quota.enabled"), the spelling the condition
	// message uses; "[]" marks an array's items and ".*" a map's values.
	Path string `json:"path"`

	// Schema is the default the schema declares at Path. Absent for valuesValueInvalid, where the
	// difference is not between two defaults.
	// +optional
	Schema *apiextensionsv1.JSON `json:"schema,omitempty"`

	// Values is values.yaml's value at Path. Absent for schemaDefaultWithoutValuesKey.
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// Kind is valueDiffers, schemaDefaultWithoutValuesKey or valuesValueInvalid.
	// +kubebuilder:validation:Enum=valueDiffers;schemaDefaultWithoutValuesKey;valuesValueInvalid
	Kind SchemaDefaultKind `json:"kind"`
}
