package monitorpair

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "pairs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBindingIsOneToOneAndRestartStable(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first, err := s.Bind(ctx, "parent1", "monitor1", "pocket-aide")
	if err != nil {
		t.Fatal(err)
	}
	// A restarted monitor (or a retry after a lost response) must land on the
	// same binding rather than make a second one.
	again, err := s.Bind(ctx, "parent1", "monitor1", "pocket-aide")
	if err != nil {
		t.Fatalf("re-binding the same pair must be a no-op: %v", err)
	}
	if !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("re-bind moved createdAt: %s -> %s", first.CreatedAt, again.CreatedAt)
	}
	for _, tc := range []struct{ parent, monitor, why string }{
		{"parent1", "monitor2", "parent already has a monitor"},
		{"parent2", "monitor1", "monitor already serves a parent"},
		{"parent3", "parent3", "a session cannot monitor itself"},
	} {
		if _, err := s.Bind(ctx, tc.parent, tc.monitor, "pocket-aide"); !errors.Is(err, ErrBound) {
			t.Fatalf("%s: Bind(%s, %s) = %v, want ErrBound", tc.why, tc.parent, tc.monitor, err)
		}
	}
}

func TestForResolvesRoleAndRefusesStrangers(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Bind(ctx, "parent1", "monitor1", "pocket-aide"); err != nil {
		t.Fatal(err)
	}
	b, role, err := s.For(ctx, "parent1")
	if err != nil || role != RoleParent || b.MonitorSessionID != "monitor1" || b.Tenant != "pocket-aide" {
		t.Fatalf("For(parent1) = %+v, %q, %v", b, role, err)
	}
	b, role, err = s.For(ctx, "monitor1")
	if err != nil || role != RoleMonitor || b.ParentSessionID != "parent1" {
		t.Fatalf("For(monitor1) = %+v, %q, %v", b, role, err)
	}
	// A session in no binding resolves to nothing — it cannot borrow someone
	// else's pair by asking.
	if _, _, err := s.For(ctx, "stranger"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("For(stranger) = %v, want ErrNotFound", err)
	}
	if _, _, err := s.For(ctx, "bad id"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("For(bad id) = %v, want ErrInvalidID", err)
	}
}

func TestSeparateBindingsStayApart(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Bind(ctx, "parentA", "monitorA", "pocket-aide"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Bind(ctx, "parentB", "monitorB", "homelab"); err != nil {
		t.Fatal(err)
	}
	if b, _, err := s.For(ctx, "parentA"); err != nil || b.MonitorSessionID != "monitorA" {
		t.Fatalf("parentA resolved to %+v, %v", b, err)
	}
	if b, _, err := s.For(ctx, "parentB"); err != nil || b.MonitorSessionID != "monitorB" || b.Tenant != "homelab" {
		t.Fatalf("parentB resolved to %+v, %v", b, err)
	}
}
