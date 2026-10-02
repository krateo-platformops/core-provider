package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/krateo-platformops/core-provider/internal/tools/authn"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// authnClientFor returns a process-wide authn client for the given URL. It is a singleton so the
// issued JWT is cached across reconciles (the exchange is amortised), mirroring how the CDC holds a
// long-lived authn client. The authn URL is stable (env-driven), so the first-seen URL wins.
var (
	authnOnce sync.Once
	authnInst *authn.Client
)

func authnClientFor(server string) *authn.Client {
	authnOnce.Do(func() { authnInst = authn.New(server, "") })
	return authnInst
}

// When a CompositionDefinition declares an apiRef, the referenced RESTAction is resolved by
// snowplow under the per-composition group krateo:cdc:<resource>-<apiVersion>. This file generates
// the RBAC that authorizes that group's in-cluster reads, by asking snowplow's dispatch-free
// GET /rbac which reads the RESTAction would need (snowplow PR #44), then granting exactly those
// rows — per-row verbs, never "*". It mirrors the authn ServiceAccount mapping lifecycle
// (apply+hash on deploy, deterministic digest contribution, delete on undeploy).
//
// Phase 1: a single cluster-scoped ClusterRole + binding per composition (fixed name), so the
// lifecycle keys off one object pair. Per-namespace Roles are a Phase 3 refinement.
// Design: docs/design/apiref-rbac-generation.md.

const restActionRBACSuffix = "-restaction"

func restActionRBACName(saName string) string { return saName + restActionRBACSuffix }

// decodeExtras parses the JSON-serialized apiRef.extras into the map the /rbac client expects.
func decodeExtras(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("decoding apiRef extras: %w", err)
	}
	return m, nil
}

// restActionReadSet calls snowplow GET /rbac for the declared apiRef. Returns (nil, nil) without an
// apiRef. Propagates restactionrbac.ErrIncomplete on a 422 so the caller can fail loud.
func restActionReadSet(ctx context.Context, opts DeployOptions) ([]restactionrbac.Resource, error) {
	if opts.ApiRefName == "" {
		return nil, nil
	}
	if opts.SnowplowURL == "" {
		return nil, fmt.Errorf("apiRef RBAC: snowplow URL not configured (set CORE_PROVIDER_SNOWPLOW_URL)")
	}
	extras, err := decodeExtras(opts.ApiRefExtras)
	if err != nil {
		return nil, err
	}
	cli := restactionrbac.New(opts.SnowplowURL)
	// snowplow's /rbac is gated by the same JWT middleware as /call: authenticate with an
	// authn-issued service JWT (core-provider presents its own projected SA token to authn),
	// mirroring how the CDC reaches snowplow for apiRef resolution. Without an authn URL the
	// request is unauthenticated and snowplow answers 401.
	if opts.AuthnURL != "" {
		cli = cli.WithToken(authnClientFor(opts.AuthnURL).Token)
	}
	return cli.Inspect(ctx, restactionrbac.Params{
		APIRefName:      opts.ApiRefName,
		APIRefNamespace: opts.ApiRefNamespace,
		Extras:          extras,
	})
}

// hashRestActionRBACObjects contributes the ClusterRole + binding to the digest with the same
// fields installRBACResources/lookupRBACResources use, so Deploy (rendered) and Lookup (live)
// agree when in sync.
func hashRestActionRBACObjects(cr *rbacv1.ClusterRole, crb *rbacv1.ClusterRoleBinding, hsh *hasher.ObjectHash) error {
	if err := hsh.SumHash(cr.Name, cr.Namespace, cr.Rules); err != nil {
		return fmt.Errorf("hashing apiRef ClusterRole: %w", err)
	}
	if err := hsh.SumHash(crb.Name, crb.Namespace, crb.Subjects, crb.RoleRef); err != nil {
		return fmt.Errorf("hashing apiRef ClusterRoleBinding: %w", err)
	}
	return nil
}

// applyRestActionRBAC resolves the apiRef read-set and grants it to the per-composition group via a
// ClusterRole + binding, applying and hashing them. No-op without apiRef. Returns
// restactionrbac.ErrIncomplete (snowplow 422) so the caller sets ApiRefRBACIncomplete instead of
// writing partial RBAC. In dry-run the apply is a server dry-run (applyOpts); the digest still
// reflects the generated objects.
func applyRestActionRBAC(ctx context.Context, dyn dynamic.Interface, opts DeployOptions, saName string, hsh *hasher.ObjectHash, applyOpts kubecli.ApplyOptions) error {
	if opts.ApiRefName == "" {
		return nil
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	readSet, err := restActionReadSet(ctx, opts)
	if err != nil {
		return err // ErrIncomplete propagates
	}

	// Before executing the RESTAction's api steps, snowplow reads the RESTAction OBJECT itself under
	// the per-composition group identity — but /rbac's read-set only covers the api steps' targets,
	// not the RESTAction. Without a `get restactions` grant the resolve 403s ("cannot get
	// restactions.templates.krateo.io"). Add it explicitly so the group can read what it resolves.
	readSet = append(readSet, restactionrbac.Resource{
		Group:    "templates.krateo.io",
		Resource: "restactions",
		Verb:     "get",
	})

	// Non-read rows are refused by the generator (#149): a userAccessFilter asks "may this user do
	// X?", and copying its verb into a Role would answer that by GRANTING X to the group being
	// asked about. Surface what was refused so an author can see why a picker's check produced no
	// grant — resource and verb only, never a user identity.
	if d := restactionrbac.NonGrantable(readSet); d.Count > 0 {
		log.Info("apiRef RBAC: refused non-read rows from the read-set; a userAccessFilter check is not a grant",
			"count", d.Count, "rows", d.Resources)
	}

	cr, crb := restactionrbac.GenerateClusterScoped(readSet, cdcGroup(saName), restActionRBACName(saName))
	if cr == nil || crb == nil {
		// Every row was non-read, so there is nothing to grant. Emitting a binding to an empty role
		// would read as an intended grant someone had broken.
		log.Info("apiRef RBAC: no grantable rows in the read-set; no ClusterRole emitted", "name", restActionRBACName(saName))
		return nil
	}

	if err := kubecli.Apply(ctx, dyn, clusterRoleGVR, cr, applyOpts); err != nil {
		log.Error(err, "installing apiRef ClusterRole", "name", cr.Name)
		return fmt.Errorf("installing apiRef ClusterRole: %w", err)
	}
	if err := kubecli.Apply(ctx, dyn, clusterRoleBindingGVR, crb, applyOpts); err != nil {
		log.Error(err, "installing apiRef ClusterRoleBinding", "name", crb.Name)
		return fmt.Errorf("installing apiRef ClusterRoleBinding: %w", err)
	}
	log.Debug("apiRef RBAC successfully installed", "name", cr.Name, "rules", len(cr.Rules))

	return hashRestActionRBACObjects(cr, crb, hsh)
}

// lookupRestActionRBAC contributes the LIVE apiRef ClusterRole + binding to the digest, mirroring
// lookupRBACResources: a not-found object hashes as empty, so a deleted/missing grant shows as
// drift. No-op without apiRef. Unlike Deploy, it makes no snowplow call.
func lookupRestActionRBAC(ctx context.Context, kube client.Client, opts DeployOptions, saName string, hsh *hasher.ObjectHash) error {
	if opts.ApiRefName == "" {
		return nil
	}
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())
	name := restActionRBACName(saName)

	cr := &rbacv1.ClusterRole{}
	if err := kube.Get(ctx, client.ObjectKey{Name: name}, cr); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("fetching apiRef ClusterRole: %w", err)
		}
		log.Debug("apiRef ClusterRole not found", "name", name)
		cr = &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	crb := &rbacv1.ClusterRoleBinding{}
	if err := kube.Get(ctx, client.ObjectKey{Name: name}, crb); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("fetching apiRef ClusterRoleBinding: %w", err)
		}
		crb = &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	return hashRestActionRBACObjects(cr, crb, hsh)
}

// deleteRestActionRBAC removes the apiRef ClusterRole + binding on undeploy. Not-found / no-CRD is
// tolerated so it never blocks teardown.
func deleteRestActionRBAC(ctx context.Context, kube client.Client, saName string) error {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())
	name := restActionRBACName(saName)

	for _, obj := range []client.Object{
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}},
	} {
		if err := kube.Delete(ctx, obj); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				continue
			}
			log.Error(err, "deleting apiRef RBAC", "name", name)
			return fmt.Errorf("deleting apiRef RBAC: %w", err)
		}
	}
	log.Debug("apiRef RBAC deleted", "name", name)
	return nil
}
