//go:build envtest

package compositiondefinitions

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
)

// core-provider#649 on a real apiserver, through provider-runtime's reconciler and the real connector:
// a verdict Observe reached and then left with an error is the one that is served, or it is not a
// verdict. docs/cd-ready-reasons.md declares the reasons; the runtime replaces Ready with NotObserved on
// an Observe error that is not marked ReadyAssessed.

// publicHost649 is a name that is not cluster-local and resolves to the test's own chart server, so a
// chart is fetched from it and the remote seed accepts it.
const publicHost649 = "charts.public.test"

// resolvePublicHost makes this process resolve publicHost649 to 127.0.0.1 and nothing else differently
// that the test uses (everything else is an IP address or localhost).
func resolvePublicHost(t *testing.T) {
	t.Helper()
	old := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		c, s := net.Pipe()
		go answerDNS649(s)
		return c, nil
	}}
	t.Cleanup(func() { net.DefaultResolver = old })
}

// answerDNS649 is a DNS server over one stream: publicHost649's A record is 127.0.0.1, every other
// question is answered NXDOMAIN, or with no record for another type.
func answerDNS649(c net.Conn) {
	defer func() { _ = c.Close() }()
	for {
		var n uint16
		if binary.Read(c, binary.BigEndian, &n) != nil {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(c, q); err != nil || n < 17 {
			return
		}
		var labels []string
		i := 12
		for i < len(q) && q[i] != 0 {
			l := int(q[i])
			labels = append(labels, string(q[i+1:i+1+l]))
			i += 1 + l
		}
		end := i + 5 // the root label, the type and the class
		isA := binary.BigEndian.Uint16(q[i+1:]) == 1
		known := strings.Join(labels, ".") == publicHost649
		r := append([]byte{}, q[:end]...)
		r[2], r[3] = 0x81, 0x80
		for j := 6; j < 12; j++ {
			r[j] = 0
		}
		switch {
		case known && isA:
			r[7] = 1
			r = append(r, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 127, 0, 0, 1)
		case !known:
			r[3] |= 3 // NXDOMAIN
		}
		if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(r))), r...)); err != nil {
			return
		}
	}
}

// publicURL is the chart server's URL under publicHost649.
func (e *env644) publicURL(name, version string) string {
	_, port, _ := net.SplitHostPort(e.charts.srv.Listener.Addr().String())
	return "http://" + publicHost649 + ":" + port + "/" + name + "-" + version + ".tgz"
}

// remote makes remote definitions deployable: the spoke is this cluster, the chart-inspector the seed
// projects onto it exists, publicHost649 resolves, and a remote definition's controller, which runs in
// the definition's own namespace, becomes available.
func (e *env644) remote(t *testing.T) {
	t.Helper()
	resolvePublicHost(t)
	e.spokeIsThisCluster(t, "")
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go markAvailable(context.Background(), e.c.cl, self642CDNS, stop)
	coords, err := deploy.ChartInspectorCoordsFromConfigmapTemplate(CDCtemplateConfigmapPath)
	if err != nil {
		t.Fatal(err)
	}
	meta := metav1.ObjectMeta{Name: coords.Name, Namespace: coords.Namespace}
	l := map[string]string{"app": coords.Name}
	for _, o := range []client.Object{
		&corev1.ServiceAccount{ObjectMeta: meta},
		&appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: l},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: l}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}}}}},
		&corev1.Service{ObjectMeta: meta, Spec: corev1.ServiceSpec{Selector: l, Ports: []corev1.ServicePort{{Port: coords.Port}}}},
	} {
		if err := e.c.cl.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
}

func onSpoke649(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
	cd.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"}}
}

// point sets the chart's URL and version.
func (d *cd644) point(url, version string) {
	d.t.Helper()
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.Chart.Url, cd.Spec.Chart.Version = url, version
	})
}

// refused asserts the served refusal: False/RemoteChartUnreachable with a message naming the chart, the
// error on Synced, and the generation it is about.
func (d *cd644) refused(when string, gen int64) {
	d.t.Helper()
	d.want(when, gen, gen, "False/RemoteChartUnreachable")
	if m := d.message("Ready"); !strings.Contains(m, d.cd().Spec.Chart.Url) || !strings.Contains(m, "cluster-local") {
		d.t.Errorf("%s: Ready message %q does not name the chart and why", when, m)
	}
	if d.cond("Synced") != "False/ReconcileError" || !strings.Contains(d.message("Synced"), "cluster-local") {
		d.t.Errorf("%s: Synced = %s (%q), want the error", when, d.cond("Synced"), d.message("Synced"))
	}
}

// A NEW remote definition whose chart is cluster-local to the management cluster. Create's own pass reads
// Creating, which the runtime writes with every Create outcome; every pass after it serves the refusal.
// When the chart is moved to a URL the spoke reaches, Ready recovers by itself.
func TestEnvtest_649_NewRemoteDefinition(t *testing.T) {
	e := start644(t)
	e.remote(t)
	e.charts.add(t, "remotelocal", "1.0.0", `{"type":"object"}`) // served from 127.0.0.1: cluster-local
	d := e.define(t, "remotelocal", "remotelocal", onSpoke649)
	w := d.watch()
	if _, err := d.pass(); err == nil {
		t.Fatal("premise: Create must fail on the seed")
	}
	d.want("Create's pass", 1, 0, "False/Creating")
	for i := 0; i < 3; i++ {
		if _, err := d.pass(); err == nil || !strings.Contains(err.Error(), "cluster-local") {
			t.Fatalf("premise: the pass must still fail, so the runtime retries it: %v", err)
		}
		d.refused("refused", 1)
	}
	rv := d.cd().ResourceVersion
	if _, err := d.pass(); err == nil || d.cd().ResourceVersion != rv {
		t.Errorf("a steady refusal is written again on every pass: resourceVersion %s -> %s (err %v)", rv, d.cd().ResourceVersion, err)
	}

	// It clears: the same chart from a URL the spoke reaches.
	d.point(e.publicURL("remotelocal", "1.0.0"), "1.0.0")
	d.want("moved, not reconciled", 2, 1, "False/RemoteChartUnreachable")
	d.rest(6)
	d.want("reachable", 2, 2, "True/Available")
	if d.cond("Synced") != "True/ReconcileSuccess" {
		t.Errorf("Synced = %s (%q)", d.cond("Synced"), d.message("Synced"))
	}
	w.holds()
	for i, s := range w.served() {
		if s.readyReason == "NotObserved" {
			t.Errorf("served version %d reads NotObserved: the verdict was overwritten", i)
		}
	}
}

// A BUMP of an existing remote definition to a chart the spoke cannot reach. Observe hands it to Update
// keeping Ready (#395), and Update's seed refuses: that refusal is what is served, from that pass on.
// #395 keeps Ready while an update RUNS; a refusal is not an update in progress, and the sibling refusals
// (SchemaDefaultContradiction, DerivedNameTooLong) say so for a definition that worked before.
func TestEnvtest_649_BumpOfAnExistingRemoteDefinition(t *testing.T) {
	e := start644(t)
	e.remote(t)
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		e.charts.add(t, "remotebump", v, `{"type":"object"}`)
	}
	d := e.define(t, "remotebump", "remotebump", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		onSpoke649(cd)
		cd.Spec.Chart.Url = e.publicURL("remotebump", "1.0.0")
	})
	d.rest(6)
	d.want("at rest at 1.0.0", 1, 1, "True/Available")

	w := d.watch()
	d.point(e.charts.url("remotebump", "2.0.0"), "2.0.0")
	d.want("bumped, not reconciled", 2, 1, "True/Available")
	for i := 1; i <= 3; i++ { // the first through Update, the others through Observe
		if _, err := d.pass(); err == nil {
			t.Fatal("premise: the pass must fail")
		}
		d.refused("refused", 2)
		if v := d.cd().Status.ApiVersion; !strings.HasSuffix(v, "/v1-0-0") {
			t.Errorf("status.apiVersion = %s: the controller still serves 1.0.0", v)
		}
	}
	d.point(e.publicURL("remotebump", "3.0.0"), "3.0.0")
	// 2.0.0 was added to the CRD and is vacated by this bump, so the add of 3.0.0 waits out its grace.
	// Ready is KEPT meanwhile, about the spec before: the field says so.
	if _, err := d.pass(); err != nil {
		t.Fatal(err)
	}
	d.want("waiting out the grace", 3, 2, "False/RemoteChartUnreachable")
	cd := d.cd()
	if len(cd.Status.ServedVersionsPendingPrune) == 0 {
		t.Fatal("premise: the add must wait out the grace of the refused version")
	}
	for i := range cd.Status.ServedVersionsPendingPrune {
		cd.Status.ServedVersionsPendingPrune[i].EligibleSince = metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second))
	}
	if err := e.c.cl.Status().Update(context.Background(), cd); err != nil {
		t.Fatal(err)
	}
	d.rest(6)
	d.want("at rest at 3.0.0", 3, 3, "True/Available")
	w.holds()
}

// The same version moved to a cluster-local URL: no version to add, so Observe's own seed refuses, and
// the same URL moved back clears it.
func TestEnvtest_649_SameVersionMovedToAnUnreachableURL(t *testing.T) {
	e := start644(t)
	e.remote(t)
	e.charts.add(t, "remotemove", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "remotemove", "remotemove", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		onSpoke649(cd)
		cd.Spec.Chart.Url = e.publicURL("remotemove", "1.0.0")
	})
	d.rest(6)
	d.point(e.charts.url("remotemove", "1.0.0"), "1.0.0")
	for i := 0; i < 2; i++ {
		_, _ = d.pass()
		d.refused("moved", 2)
	}
	d.point(e.publicURL("remotemove", "1.0.0"), "1.0.0")
	d.rest(6)
	d.want("moved back", 3, 3, "True/Available")
}

// The seed's OTHER failures are not the refusal: the spoke could not be seeded, so Ready is not known and
// nothing is recorded. CONTROL for the mark and the record being on the refusal alone.
func TestEnvtest_649_OtherSeedFailuresAreNotAssessed(t *testing.T) {
	e := start644(t)
	e.remote(t)
	e.charts.add(t, "remoteseed", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "remoteseed", "remoteseed", func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		onSpoke649(cd)
		cd.Spec.Chart.Url = e.publicURL("remoteseed", "1.0.0")
	})
	d.rest(6)
	// The chart-inspector to project is gone from the hub.
	coords, err := deploy.ChartInspectorCoordsFromConfigmapTemplate(CDCtemplateConfigmapPath)
	if err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: coords.Name, Namespace: coords.Namespace}}
	if err := e.c.cl.Delete(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) { cd.Spec.UpgradePolicy = "Manual" })
	if _, err := d.pass(); err == nil || !strings.Contains(err.Error(), "chart-inspector") {
		t.Fatalf("premise: the seed must fail on the projection, got %v", err)
	}
	d.want("the projection failed", 2, 1, "Unknown/NotObserved")
}

// ---- the enumeration: every other place that sets a condition and then leaves Observe with an error ----

// An unreadable chart whose CRD read fails too (observeWithoutChart). The chart is unreadable whatever
// the CRD read says, and Unavailable's row declares it, so it is served. On a definition being deleted
// the runtime's Deleting is.
func TestEnvtest_649_UnreadableChartAndCRD(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "unreadcrd")
	d.repoint("9.9.9")
	e.kube.onGet(func(o client.Object) error {
		if _, ok := o.(*apiextensionsv1.CustomResourceDefinition); ok {
			return errors.New("injected: the CRD cannot be read")
		}
		return nil
	})
	for i := 0; i < 2; i++ {
		if _, err := d.pass(); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("premise: the CRD read must fail the pass, got %v", err)
		}
		d.want("chart and CRD unreadable", 2, 2, "False/Unavailable")
		if m := d.message("Ready"); !strings.Contains(m, "chart could not be read") {
			t.Errorf("Ready message = %q", m)
		}
	}
	// CONTROL: the same CRD failure with a readable chart determined nothing.
	d.repoint("1.0.0")
	_, _ = d.pass()
	d.want("only the CRD unreadable", 3, 2, "Unknown/NotObserved")

	d.repoint("9.9.9")
	if err := e.c.cl.Delete(context.Background(), d.cd()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pass(); err == nil {
		t.Fatal("premise: the pass must still fail")
	}
	if got := d.cond("Ready"); got != "False/Deleting" {
		t.Errorf("a definition being deleted: Ready = %s, want False/Deleting", got)
	}
	if _, obs := d.generations(); obs != 2 {
		t.Errorf("a definition being deleted: observedGeneration %d, want it frozen at 2", obs)
	}
	e.kube.onGet(nil)
}

// The seed of a first pass (Creating, before any work) is not a verdict: an Observe that then errors
// could not tell, and NotObserved is served, which is the row for it.
func TestEnvtest_649_SeededReadyIsNotAVerdict(t *testing.T) {
	e := start644(t)
	e.charts.add(t, "seeded", "1.0.0", `{"type":"object"}`)
	d := e.define(t, "seeded", "seeded", nil)
	e.kube.onGet(func(o client.Object) error {
		if _, ok := o.(*apiextensionsv1.CustomResourceDefinition); ok {
			return errors.New("injected: the CRD cannot be read")
		}
		return nil
	})
	if _, err := d.pass(); err == nil {
		t.Fatal("premise: the first Observe must fail")
	}
	d.want("first pass failed", 1, 0, "Unknown/NotObserved")
	e.kube.onGet(nil)
	d.rest(6)
}

// A follower that has just become the owner says Creating and goes on to look at what it owns. An error
// there means it could not tell, so NotObserved is served and the follower's Ready=True is not.
func TestEnvtest_649_BecomingOwnerThenFailing(t *testing.T) {
	e := start644(t)
	owner := e.settled(t, "handed")
	f := e.define(t, "handed", "handed-follower", nil)
	f.rest(6)
	if f.cd().Status.Owner == nil {
		t.Fatal("premise: the second definition must be a follower")
	}
	if err := e.c.cl.Delete(context.Background(), owner.cd()); err != nil {
		t.Fatal(err)
	}
	good := CDCtemplateDeploymentPath
	t.Cleanup(func() { CDCtemplateDeploymentPath = good })
	CDCtemplateDeploymentPath = filepath.Join(t.TempDir(), "gone.yaml")
	if _, err := f.pass(); err == nil {
		t.Fatal("premise: the new owner's dry run must fail")
	}
	if f.cd().Status.Owner != nil {
		t.Fatal("premise: the follower must have become the owner")
	}
	f.want("became the owner, and could not tell", 1, 1, "Unknown/NotObserved")
	CDCtemplateDeploymentPath = good
}

// The conditions other than Ready are not the runtime's: what a failing Observe set on them is served.
// An unreadable controller Deployment, with ControllerActive beside the Ready it replaces, and the
// apiRef and authn conditions of a pass authn refuses.
func TestEnvtest_649_OtherConditionsSurviveTheError(t *testing.T) {
	e := start644(t)
	d := e.settled(t, "others")
	e.kube.onGet(func(o client.Object) error {
		if _, ok := o.(*appsv1.Deployment); ok {
			return errors.New("injected: the Deployment cannot be read")
		}
		return nil
	})
	if _, err := d.pass(); err == nil {
		t.Fatal("premise: the pass must fail")
	}
	d.want("Deployment unreadable", 1, 1, "Unknown/NotObserved")
	if got := d.cond("ControllerActive"); got != "Unknown/NotObserved" {
		t.Errorf("ControllerActive = %s, want Unknown/NotObserved", got)
	}
	e.kube.onGet(nil)
	d.rest(2)

	d.edit(func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) {
		cd.Spec.ApiRef = &compositiondefinitionsv1alpha1.ApiReference{Name: "status-sources", Namespace: self642CDNS}
	})
	if _, err := d.pass(); err == nil {
		t.Fatal("premise: authn is not installed, so the pass must fail")
	}
	d.want("authn missing", 2, 1, "Unknown/NotObserved")
	if got := d.cond("AuthnMappingApplied"); got != "False/AuthnNotInstalled" {
		t.Errorf("AuthnMappingApplied = %s, want False/AuthnNotInstalled", got)
	}
	if got := d.cond("ApiRefRBACComplete"); !strings.HasPrefix(got, "False/") {
		t.Errorf("ApiRefRBACComplete = %s, want its failure", got)
	}
}
