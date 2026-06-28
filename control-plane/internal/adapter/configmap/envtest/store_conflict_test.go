// Package envtest_test exercises the ConfigMap adapter against a REAL
// kube-apiserver + etcd (controller-runtime's envtest), not the fake clientset.
//
// Why a real API server: the fake clientset's object tracker does not enforce
// resourceVersion optimistic concurrency — its Update blindly replaces the
// stored object — so it cannot reproduce a genuine CompareAndSwapState race
// (both writers would "win"). Only a real API server rejects the second writer
// with a 409 Conflict. These tests assert the single-winner property AC-C1
// depends on: under concurrent transitions / lock acquisitions on one session,
// exactly one succeeds and the rest get session.ErrConflict.
//
// The suite skips unless KUBEBUILDER_ASSETS points at the apiserver/etcd
// binaries; `make test-envtest` provisions them via setup-envtest.
package envtest_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const testNamespace = "session-conflict-test"

// startEnv boots a real control plane (apiserver + etcd) and returns a clientset
// for it plus a stop func. It skips when the envtest binaries are not available.
func startEnv(t *testing.T) (kubernetes.Interface, func()) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest: KUBEBUILDER_ASSETS unset — run via `make test-envtest` (provisions kube-apiserver + etcd)")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest control plane: %v", err)
	}
	stop := func() {
		if err := env.Stop(); err != nil {
			t.Logf("stop envtest: %v", err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		stop()
		t.Fatalf("build clientset: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}},
		metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		stop()
		t.Fatalf("create namespace: %v", err)
	}
	return cs, stop
}

// tally classifies the results of N racing operations into successes / conflicts
// and fails on any other error.
func tally(t *testing.T, results []error) (wins, conflicts int) {
	t.Helper()
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case err == session.ErrConflict:
			conflicts++
		default:
			t.Fatalf("unexpected error from racing op: %v", err)
		}
	}
	return wins, conflicts
}

// race runs fn(i) in n goroutines released simultaneously, returning each result.
func race(n int, fn func(i int) error) []error {
	results := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

// AC-C1: concurrent CompareAndSwapState on one session converges to a single
// winner — the real API server's resourceVersion check rejects all but one
// active->idle transition, so state never tears.
func TestCAS_SingleWinnerUnderConcurrency(t *testing.T) {
	cs, stop := startEnv(t)
	defer stop()
	ctx := context.Background()
	store := configmap.NewStore(cs, testNamespace)

	const id = "cas-race"
	if err := store.Put(ctx, &session.Session{ID: id, Name: id, State: session.StateActive}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	const n = 8
	results := race(n, func(int) error {
		return store.CompareAndSwapState(ctx, id, session.StateActive, session.StateIdle)
	})

	wins, conflicts := tally(t, results)
	if wins != 1 {
		t.Fatalf("CAS winners=%d want exactly 1 (single atomic transition, AC-C1)", wins)
	}
	if conflicts != n-1 {
		t.Fatalf("CAS conflicts=%d want %d", conflicts, n-1)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get after race: %v", err)
	}
	if got.State != session.StateIdle {
		t.Fatalf("final state=%q want idle (the one winning transition)", got.State)
	}
}

// AC-C1: concurrent Lock acquisitions on one session yield a single holder —
// the API server admits exactly one Lease Create, so occupancy is exclusive
// across replicas.
func TestLock_SingleWinnerUnderConcurrency(t *testing.T) {
	cs, stop := startEnv(t)
	defer stop()
	ctx := context.Background()
	store := configmap.NewStore(cs, testNamespace)

	const id = "lock-race"
	const n = 8
	results := race(n, func(i int) error {
		return store.Lock(ctx, id, fmt.Sprintf("token-%d", i))
	})

	wins, conflicts := tally(t, results)
	if wins != 1 {
		t.Fatalf("Lock winners=%d want exactly 1 (exclusive occupancy, AC-C1)", wins)
	}
	if conflicts != n-1 {
		t.Fatalf("Lock conflicts=%d want %d", conflicts, n-1)
	}
}
