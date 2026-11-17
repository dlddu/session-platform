// Package observability holds the control plane's own measurements of the
// provisioning rounds — the create and restore paths that start, reclaim and
// commit session pods.
//
// Those paths used to emit nothing at all: a round that reclaimed a pod it had
// just built left no log line and no counter, so the only surviving record of
// why was whatever an unrelated caller happened to log. The package exists so
// that the next failure is answerable from this process.
//
// Labels here are a closed set by construction. Session ids, pod names and
// request ids are the natural keys for *reading* one failure, but each of them
// is unbounded, and a label with unbounded values multiplies one series into
// one per session forever. They belong in the structured logs, which are
// keyed, searchable and dropped on a retention schedule; what stays here is
// the small fixed vocabulary a dashboard or an alert can sum over.
package observability

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Round outcomes. A round ends in exactly one of these, and the set is closed:
// an unrecognised value folds to ResultOther rather than opening a new series.
const (
	ResultOK             = "ok"
	ResultOwnershipLost  = "ownership-lost"
	ResultValidityLost   = "validity-lost"
	ResultCancelled      = "cancelled"
	ResultWorkloadFailed = "workload-failed"
	ResultStoreFailed    = "store-failed"
	ResultOther          = "other"
)

// Why a round reclaimed the pods it had provisioned.
const (
	ReasonRoundFailed    = "round-failed"
	ReasonOwnershipLost  = "ownership-lost"
	ReasonCommitRejected = "commit-rejected"
	ReasonOther          = "other"
)

// How a lease renewal ended, for the attempts that did not simply succeed.
const (
	RenewUnconfirmed   = "unconfirmed"
	RenewOwnershipLost = "ownership-lost"
	RenewValidityLost  = "validity-lost"
	RenewOther         = "other"
)

// Operations that take a per-session lease for a whole round.
const (
	OpCreate  = "create"
	OpRestore = "restore"
)

var (
	results    = closedSet(ResultOther, ResultOK, ResultOwnershipLost, ResultValidityLost, ResultCancelled, ResultWorkloadFailed, ResultStoreFailed)
	reasons    = closedSet(ReasonOther, ReasonRoundFailed, ReasonOwnershipLost, ReasonCommitRejected)
	renewKinds = closedSet(RenewOther, RenewUnconfirmed, RenewOwnershipLost, RenewValidityLost)
	operations = closedSet("other", OpCreate, OpRestore)
)

// roundBuckets are seconds. A round spans a node scale-up and an image pull,
// so the interesting range is tens of seconds to minutes, not milliseconds.
var roundBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600}

// Metrics is safe for concurrent use and allocates nothing per observation
// beyond the first time a label combination is seen; the combinations are
// bounded by the closed sets above.
type Metrics struct {
	mu            sync.Mutex
	rounds        map[[2]string]uint64
	roundSeconds  map[string]*histogram
	leaseRenews   map[[2]string]uint64
	podsReclaimed map[string]uint64
}

// NewMetrics returns an empty registry. A nil *Metrics is usable and discards
// everything, so wiring it is optional for callers that do not expose it.
func NewMetrics() *Metrics {
	return &Metrics{
		rounds:        map[[2]string]uint64{},
		roundSeconds:  map[string]*histogram{},
		leaseRenews:   map[[2]string]uint64{},
		podsReclaimed: map[string]uint64{},
	}
}

// RoundFinished records one completed provisioning round and how long it took.
func (m *Metrics) RoundFinished(operation, result string, d time.Duration) {
	if m == nil {
		return
	}
	op, res := operations.fold(operation), results.fold(result)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rounds[[2]string{op, res}]++
	h, ok := m.roundSeconds[op]
	if !ok {
		h = newHistogram(roundBuckets)
		m.roundSeconds[op] = h
	}
	h.observe(d.Seconds())
}

// LeaseRenewFailed records a renewal attempt that did not succeed, by verdict.
// Unconfirmed attempts are counted too: a round that survives on retries is
// invisible otherwise, and the rate of them is the early warning.
func (m *Metrics) LeaseRenewFailed(kind string) {
	if m == nil {
		return
	}
	k := renewKinds.fold(kind)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leaseRenews[[2]string{k, ""}]++
}

// PodsReclaimed records pods a round deleted after failing, by why.
func (m *Metrics) PodsReclaimed(reason string, n int) {
	if m == nil || n <= 0 {
		return
	}
	r := reasons.fold(reason)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.podsReclaimed[r] += uint64(n)
}

// WritePrometheus renders the registry in the Prometheus text exposition
// format. It is the whole exposition surface: this package serves no HTTP and
// decides nothing about who may read it.
func (m *Metrics) WritePrometheus(w io.Writer) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := &strings.Builder{}
	writeHeader(b, "session_platform_provisioning_rounds_total", "counter",
		"Provisioning rounds that finished, by operation and outcome.")
	for _, k := range sortedPairs(m.rounds) {
		fmt.Fprintf(b, "session_platform_provisioning_rounds_total{operation=%q,result=%q} %d\n", k[0], k[1], m.rounds[k])
	}
	writeHeader(b, "session_platform_provisioning_round_seconds", "histogram",
		"How long a provisioning round took, by operation.")
	for _, op := range sortedKeys(m.roundSeconds) {
		m.roundSeconds[op].write(b, "session_platform_provisioning_round_seconds", fmt.Sprintf("operation=%q", op))
	}
	writeHeader(b, "session_platform_lease_renew_failures_total", "counter",
		"Lease renewal attempts that did not succeed, by verdict.")
	for _, k := range sortedPairs(m.leaseRenews) {
		fmt.Fprintf(b, "session_platform_lease_renew_failures_total{kind=%q} %d\n", k[0], m.leaseRenews[k])
	}
	writeHeader(b, "session_platform_pods_reclaimed_total", "counter",
		"Session pods a failed round deleted, by reason.")
	for _, r := range sortedKeys(m.podsReclaimed) {
		fmt.Fprintf(b, "session_platform_pods_reclaimed_total{reason=%q} %d\n", r, m.podsReclaimed[r])
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Handler serves the exposition. It is deliberately not mounted by default:
// the control plane's own API is reachable inside the cluster without
// authentication and is kept that way by a NetworkPolicy, so every additional
// route widens what an in-cluster caller can read. What this one would leak is
// bounded by the closed label sets — aggregate counts, no session or pod
// identifiers, nothing tenant-scoped — but the decision to expose it at all
// belongs to whoever runs the deployment, not to this package.
func Handler(m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := m.WritePrometheus(w); err != nil {
			http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		}
	})
}

func writeHeader(b *strings.Builder, name, typ, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	total  uint64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
}

func (h *histogram) observe(v float64) {
	h.sum += v
	h.total++
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
}

func (h *histogram) write(b *strings.Builder, name, labels string) {
	for i, bound := range h.bounds {
		fmt.Fprintf(b, "%s_bucket{%s,le=%q} %d\n", name, labels, strconv.FormatFloat(bound, 'g', -1, 64), h.counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, labels, h.total)
	fmt.Fprintf(b, "%s_sum{%s} %s\n", name, labels, strconv.FormatFloat(h.sum, 'g', -1, 64))
	fmt.Fprintf(b, "%s_count{%s} %d\n", name, labels, h.total)
}

// set is a closed label vocabulary: anything outside it folds to the member
// named first, so a caller passing a new string cannot create a new series.
type set struct {
	fallback string
	members  map[string]struct{}
}

func closedSet(fallback string, members ...string) set {
	m := map[string]struct{}{fallback: {}}
	for _, v := range members {
		m[v] = struct{}{}
	}
	return set{fallback: fallback, members: m}
}

func (s set) fold(v string) string {
	if _, ok := s.members[v]; ok {
		return v
	}
	return s.fallback
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPairs[V any](m map[[2]string]V) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
