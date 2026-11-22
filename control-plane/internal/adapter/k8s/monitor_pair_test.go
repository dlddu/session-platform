package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The pair token reaches the workload container and nothing else: its own
// audience, a read-only file mount, and no value in the pod spec.
func TestMonitorPairTokenGoesToTheWorkloadContainerOnly(t *testing.T) {
	o := &ClientOrchestrator{}
	WithMonitorPair("http://control-plane.sessions.svc.cluster.local/")(o)
	if o.monitorPairURL != "http://control-plane.sessions.svc.cluster.local" {
		t.Fatalf("url = %q", o.monitorPairURL)
	}
	var volumes []corev1.Volume
	workload := corev1.Container{Name: ContainerName}
	sidecar := corev1.Container{Name: K3SMCPProxyContainerName}
	o.attachMonitorPair(&volumes, &workload)

	if len(volumes) != 1 || volumes[0].Projected == nil {
		t.Fatalf("volumes = %+v, want one projected volume", volumes)
	}
	src := volumes[0].Projected.Sources
	if len(src) != 1 || src[0].ServiceAccountToken == nil {
		t.Fatalf("projection = %+v", src)
	}
	if got := src[0].ServiceAccountToken.Audience; got != MonitorPairTokenAudience {
		t.Fatalf("audience = %q, want its own %q", got, MonitorPairTokenAudience)
	}
	if got := src[0].ServiceAccountToken.Audience; got == RepoAccessTokenAudience {
		t.Fatalf("the pair token must not share repo access's audience (%q)", got)
	}
	if len(workload.VolumeMounts) != 1 || !workload.VolumeMounts[0].ReadOnly {
		t.Fatalf("workload mounts = %+v, want one read-only mount", workload.VolumeMounts)
	}
	var url, file string
	for _, e := range workload.Env {
		switch e.Name {
		case MonitorPairURLEnvVar:
			url = e.Value
		case MonitorPairTokenFileEnvVar:
			file = e.Value
		}
	}
	if url == "" || file == "" {
		t.Fatalf("workload env = %+v, want the URL and the token file", workload.Env)
	}
	if file == url || len(file) == 0 || file[0] != '/' {
		t.Fatalf("token file must be a path, got %q", file)
	}
	// The sidecar is untouched: it already holds the cluster token, and this
	// one would let it speak as the session.
	if len(sidecar.VolumeMounts) != 0 || len(sidecar.Env) != 0 {
		t.Fatalf("sidecar was touched: %+v %+v", sidecar.VolumeMounts, sidecar.Env)
	}
}

// With the feature off nothing is projected, so a deployment that has not
// opted in gives no session a path to the API.
func TestMonitorPairTokenIsAbsentWhenDisabled(t *testing.T) {
	o := &ClientOrchestrator{}
	WithMonitorPair("  ")(o)
	var volumes []corev1.Volume
	workload := corev1.Container{Name: ContainerName}
	o.attachMonitorPair(&volumes, &workload)
	if len(volumes) != 0 || len(workload.VolumeMounts) != 0 || len(workload.Env) != 0 {
		t.Fatalf("disabled feature projected something: %+v %+v %+v", volumes, workload.VolumeMounts, workload.Env)
	}
}
