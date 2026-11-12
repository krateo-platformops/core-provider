package auditrec

import (
	"github.com/krateo-platformops/audit"
	"go.opentelemetry.io/otel"
	logglobal "go.opentelemetry.io/otel/log/global"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// New builds core-provider's Recorder: records go to the process LoggerProvider under Scope (OTLP
// when logs are enabled; a no-op provider otherwise), the excluded counter to the process
// MeterProvider, and preview is read from the hub's Namespaces unless a Target names its own
// cluster. The actor is core-provider's ServiceAccount from the downward API
// (audit.ControllerActor: POD_NAMESPACE, POD_SERVICE_ACCOUNT); when either is unset no actor is
// given, and every record is marked actor-incomplete, never guessed. ok reports whether an actor
// was found, for the caller to log.
func New(hub *rest.Config) (r *Recorder, ok bool, err error) {
	dyn, err := dynamic.NewForConfig(hub)
	if err != nil {
		return nil, false, err
	}
	opts := []audit.Option{audit.WithWriter(audit.WriterController)}
	actor := audit.ControllerActor()
	if actor != "" {
		opts = append(opts, audit.WithActor(actor, audit.ClassController))
	}
	e := audit.New(logglobal.GetLoggerProvider().Logger(Scope), opts...)
	r, err = NewRecorder(e, otel.GetMeterProvider(), LabelsFrom(dyn))
	return r, actor != "", err
}

// CauseOfDefinition is the cause_ref of every write a reconcile of the CompositionDefinition cd
// makes: core.krateo.io/v1alpha1 compositiondefinitions, with cd's namespace, name, uid and the
// generation the reconcile acts on.
func CauseOfDefinition(cd client.Object) audit.CauseRef {
	return audit.CauseRef{
		Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions",
		Namespace: cd.GetNamespace(), Name: cd.GetName(), UID: string(cd.GetUID()), Generation: cd.GetGeneration(),
	}
}
