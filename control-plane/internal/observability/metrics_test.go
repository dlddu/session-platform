package observability_test

import (
	"strings"
	"testing"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/observability"
)

// TestUnknownLabelsFoldInsteadOfOpeningSeries is the property the package
// exists to guarantee. A caller that passes a session id, a pod name or any
// other unbounded string must not be able to create a series per value — the
// value folds into the vocabulary's fallback member instead.
func TestUnknownLabelsFoldInsteadOfOpeningSeries(t *testing.T) {
	m := observability.NewMetrics()
	for _, id := range []string{"e9088f832e2ac7cd", "sess-rfd26ed3fa02ae9f", "req-1", "req-2"} {
		m.RoundFinished(id, id, time.Second)
		m.LeaseRenewFailed(id)
		m.PodsReclaimed(id, 1)
	}
	out := render(t, m)
	for _, unbounded := range []string{"e9088f832e2ac7cd", "sess-rfd26ed3fa02ae9f", "req-1", "req-2"} {
		if strings.Contains(out, unbounded) {
			t.Errorf("exposition leaked the unbounded label value %q", unbounded)
		}
	}
	if got := countLines(out, `session_platform_provisioning_rounds_total{operation="other",result="other"} 4`); got != 1 {
		t.Errorf("four unknown rounds must collapse onto one folded series, got:\n%s", out)
	}
	if got := countLines(out, `session_platform_pods_reclaimed_total{reason="other"} 4`); got != 1 {
		t.Errorf("reclaim reasons did not fold, got:\n%s", out)
	}
}

// TestKnownLabelsStayDistinct is the other half: folding must not swallow the
// vocabulary the dashboards actually read.
func TestKnownLabelsStayDistinct(t *testing.T) {
	m := observability.NewMetrics()
	m.RoundFinished(observability.OpRestore, observability.ResultOK, 2*time.Second)
	m.RoundFinished(observability.OpRestore, observability.ResultValidityLost, 20*time.Second)
	m.RoundFinished(observability.OpCreate, observability.ResultOwnershipLost, time.Minute)
	m.LeaseRenewFailed(observability.RenewUnconfirmed)
	m.LeaseRenewFailed(observability.RenewUnconfirmed)
	m.LeaseRenewFailed(observability.RenewValidityLost)

	out := render(t, m)
	for _, want := range []string{
		`session_platform_provisioning_rounds_total{operation="restore",result="ok"} 1`,
		`session_platform_provisioning_rounds_total{operation="restore",result="validity-lost"} 1`,
		`session_platform_provisioning_rounds_total{operation="create",result="ownership-lost"} 1`,
		`session_platform_lease_renew_failures_total{kind="unconfirmed"} 2`,
		`session_platform_lease_renew_failures_total{kind="validity-lost"} 1`,
		`session_platform_provisioning_round_seconds_count{operation="restore"} 2`,
		`session_platform_provisioning_round_seconds_bucket{operation="restore",le="5"} 1`,
	} {
		if countLines(out, want) != 1 {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestZeroValuesAreNotRecorded keeps the exposition free of observations that
// say nothing: a reclaim of no pods is not an event.
func TestZeroValuesAreNotRecorded(t *testing.T) {
	m := observability.NewMetrics()
	m.PodsReclaimed(observability.ReasonRoundFailed, 0)
	if out := render(t, m); strings.Contains(out, "pods_reclaimed_total{") {
		t.Errorf("a zero reclaim produced a series:\n%s", out)
	}
}

// TestNilRegistryIsUsable lets a caller wire the service without deciding to
// expose anything, which is the default in main.
func TestNilRegistryIsUsable(t *testing.T) {
	var m *observability.Metrics
	m.RoundFinished(observability.OpRestore, observability.ResultOK, time.Second)
	m.LeaseRenewFailed(observability.RenewUnconfirmed)
	m.PodsReclaimed(observability.ReasonRoundFailed, 3)
	if err := m.WritePrometheus(&strings.Builder{}); err != nil {
		t.Fatalf("nil registry must discard, got %v", err)
	}
}

func render(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	b := &strings.Builder{}
	if err := m.WritePrometheus(b); err != nil {
		t.Fatalf("write: %v", err)
	}
	return b.String()
}

func countLines(out, want string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if line == want {
			n++
		}
	}
	return n
}
