package dynamic

import (
	"context"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// crdGVR is the GroupVersionResource of CustomResourceDefinition objects.
var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// crdInvalidateWindow coalesces bursts of CRD events (e.g. a Pass-A render that registers
// many component CRDs at once, or the informer's initial list) into a single discovery
// invalidation. It is a FIXED window measured from the first event of a burst (a throttle,
// not an extending debounce) so that continuous CRD churn still invalidates at least once
// per window — bounding worst-case discovery staleness to this duration.
const crdInvalidateWindow = 2 * time.Second

// WatchCRDsAndInvalidate watches CustomResourceDefinition events and invalidates the
// RESTMapper's cached discovery whenever a CRD is added, updated, or deleted.
//
// The mapper built by NewRESTMapper is a DeferredDiscoveryRESTMapper backed by a
// MemCache discovery client: it caches API groups/resources on first use and is never
// refreshed on its own. So a CRD (or a new CRD VERSION) registered after the cache
// warmed is invisible to RESTMapping/IsNamespaced — and, for the umbrella, to the
// `inst.crdExists` Pass-B gate — until the controller is restarted. This watch makes the
// refresh automatic: on any CRD change it calls mapper.Reset(), which invalidates the
// MemCache and drops the delegate so the next lookup re-discovers from the live cluster.
//
// It is a no-op (returns nil) if the mapper does not implement Reset() — e.g. a static
// mapper in tests. Setup errors are returned; once started it runs until ctx is done.
//
// # Why the METADATA client and not the dynamic one
//
// This watch is a pure change TRIGGER: the handlers below read nothing but
// resourceVersion, and the objects themselves are discarded. A dynamic informer would
// nonetheless cache every CustomResourceDefinition in full, as *unstructured.Unstructured
// — and CRDs are among the largest objects in a cluster because they carry complete
// OpenAPI schemas.
//
// That was not a theoretical cost. Profiled on krateo-057 (2026-10-22), an IDLE controller
// — 1m CPU, one composition instance — held 72.33 MiB of live heap, of which 60.23 MiB
// (83%) was this informer's cache: 540 CRDs, 38 MiB of JSON, inflating to ~60 MiB of
// decoded maps. Every CDC in the fleet paid it independently, roughly 3.6 GiB cluster-wide,
// to be told that a CRD changed (krateo-platformops/core-provider#182).
//
// metadatainformer caches PartialObjectMetadata instead — name, UID, resourceVersion and
// labels, which is a superset of what this function reads. meta.Accessor is satisfied by
// PartialObjectMetadata, so the resync-skip below is unchanged. The apiserver serves this
// projection natively (as=PartialObjectMetadataList), so the saving is on the wire as well
// as in memory, and the RBAC requirement is identical: get;list;watch on
// customresourcedefinitions.apiextensions.k8s.io.
func WatchCRDsAndInvalidate(ctx context.Context, rc *rest.Config, mapper meta.RESTMapper, log logging.Logger) error {
	resetter, ok := mapper.(interface{ Reset() })
	if !ok {
		return nil
	}

	mc, err := metadata.NewForConfig(rc)
	if err != nil {
		return err
	}

	// Buffered depth 1: many events collapse into one pending invalidation.
	trigger := make(chan struct{}, 1)
	signal := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}

	factory := metadatainformer.NewSharedInformerFactory(mc, 10*time.Minute)
	informer := factory.ForResource(crdGVR).Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { signal() },
		UpdateFunc: func(oldObj, newObj any) {
			// Skip periodic resync no-ops (same resourceVersion) to avoid needless resets.
			oldAcc, errOld := meta.Accessor(oldObj)
			newAcc, errNew := meta.Accessor(newObj)
			if errOld == nil && errNew == nil && oldAcc.GetResourceVersion() == newAcc.GetResourceVersion() {
				return
			}
			signal()
		},
		DeleteFunc: func(any) { signal() },
	}); err != nil {
		return err
	}

	go informer.Run(ctx.Done())

	// Coalescing invalidation loop: on the first trigger of a burst, open a fixed window,
	// drain any further triggers that arrive during it, then Reset() once at the end.
	go func() {
		// Surface a missing/denied watch instead of silently degrading to manual restarts.
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			if log != nil {
				log.Info("CustomResourceDefinition watch did not sync; discovery auto-invalidation inactive " +
					"(falling back to reactive reset — check RBAC: get;list;watch customresourcedefinitions.apiextensions.k8s.io)")
			}
			return
		}
		if log != nil {
			log.Info("CustomResourceDefinition watch active; RESTMapper discovery cache will auto-invalidate on CRD changes")
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
			}

			timer := time.NewTimer(crdInvalidateWindow)
		drain:
			for {
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-trigger:
					// keep draining the burst within the window
				case <-timer.C:
					break drain
				}
			}

			resetter.Reset()
			if log != nil {
				log.Debug("Invalidated RESTMapper discovery cache after CustomResourceDefinition change")
			}
		}
	}()

	return nil
}
