package repoaccess

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repo-access.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestNormalizeRepo(t *testing.T) {
	cases := map[string]string{
		"dlddu/Reconciler":         "dlddu/reconciler",
		" dlddu/session-platform ": "dlddu/session-platform",
		"dlddu/repo.git":           "dlddu/repo",
	}
	for in, want := range cases {
		got, err := NormalizeRepo(in)
		if err != nil || got != want {
			t.Errorf("NormalizeRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "dlddu", "dlddu/", "/repo", "a/b/c", "dlddu/..", "../x", "a b/c", "https://github.com/a/b"} {
		if _, err := NormalizeRepo(bad); !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("NormalizeRepo(%q) err = %v; want ErrInvalidRepo", bad, err)
		}
	}
}

func TestAddIsCaseInsensitiveAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	if _, created, err := s.Add(ctx, "dlddu/Reconciler"); err != nil || !created {
		t.Fatalf("first add: created=%v err=%v", created, err)
	}
	if _, created, err := s.Add(ctx, "DLDDU/reconciler"); err != nil || created {
		t.Fatalf("second add: created=%v err=%v; want existing entry", created, err)
	}
	_ = s.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	list, err := s2.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list after reopen = %v, %v; want one entry", list, err)
	}
	if e := list[0]; e.Repo != "dlddu/reconciler" || e.Source != SourceManual {
		t.Fatalf("entry = %+v", e)
	}
	ok, err := s2.Allowed(ctx, "Dlddu/Reconciler")
	if err != nil || !ok {
		t.Fatalf("Allowed = %v, %v", ok, err)
	}
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, _, _ = s.Add(ctx, "dlddu/a")
	if removed, err := s.Remove(ctx, "dlddu/A"); err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if removed, err := s.Remove(ctx, "dlddu/a"); err != nil || removed {
		t.Fatalf("second remove = %v, %v; want false", removed, err)
	}
	if ok, _ := s.Allowed(ctx, "dlddu/a"); ok {
		t.Fatal("still allowed after remove")
	}
}

func TestRequestDedupesAndReportsAllowed(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	r1, status, err := s.Request(ctx, "dlddu/reconciler", "sess-1", "need models")
	if err != nil || status != StatusPending || r1.ID == "" {
		t.Fatalf("first request = %+v %q %v", r1, status, err)
	}
	r2, status, err := s.Request(ctx, "DLDDU/reconciler", "sess-1", "asking again")
	if err != nil || status != StatusPending || r2.ID != r1.ID || r2.Reason != "need models" {
		t.Fatalf("repeat request = %+v %q %v; want the first request back", r2, status, err)
	}
	if _, _, err := s.Request(ctx, "dlddu/reconciler", "sess-2", "me too"); err != nil {
		t.Fatalf("other session: %v", err)
	}
	reqs, err := s.Requests(ctx)
	if err != nil || len(reqs) != 2 {
		t.Fatalf("pending = %v, %v; want 2", reqs, err)
	}

	_, _, _ = s.Add(ctx, "dlddu/open")
	r, status, err := s.Request(ctx, "dlddu/open", "sess-1", "x")
	if err != nil || status != StatusAllowed || r.ID != "" {
		t.Fatalf("request for allowed repo = %+v %q %v; want allowed, nothing recorded", r, status, err)
	}
	if reqs, _ := s.Requests(ctx); len(reqs) != 2 {
		t.Fatalf("allowed repo request was recorded: %v", reqs)
	}
}

func TestRequestValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, _, err := s.Request(ctx, "nope", "sess", "x"); !errors.Is(err, ErrInvalidRepo) {
		t.Fatalf("bad repo err = %v", err)
	}
	if _, _, err := s.Request(ctx, "a/b", "sess", "  "); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("empty reason err = %v", err)
	}
	long := make([]rune, maxReasonRunes+1)
	for i := range long {
		long[i] = '가'
	}
	if _, _, err := s.Request(ctx, "a/b", "sess", string(long)); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("long reason err = %v", err)
	}
}

func TestApproveAddsAndClearsAllRequestsForRepo(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	r1, _, _ := s.Request(ctx, "dlddu/reconciler", "sess-1", "a")
	_, _, _ = s.Request(ctx, "dlddu/reconciler", "sess-2", "b")
	_, _, _ = s.Request(ctx, "dlddu/other", "sess-1", "c")

	e, err := s.Approve(ctx, r1.ID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if e.Repo != "dlddu/reconciler" || e.Source != SourceRequest || e.RequestSessionID != "sess-1" {
		t.Fatalf("entry = %+v", e)
	}
	reqs, _ := s.Requests(ctx)
	if len(reqs) != 1 || reqs[0].Repo != "dlddu/other" {
		t.Fatalf("pending after approve = %+v; want only dlddu/other", reqs)
	}
	if _, err := s.Approve(ctx, r1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve twice err = %v; want ErrNotFound", err)
	}
}

func TestApproveOfAlreadyAllowedRepoKeepsOriginalEntry(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	r, _, _ := s.Request(ctx, "dlddu/x", "sess-1", "a")
	_, _, _ = s.Add(ctx, "dlddu/x")
	reqs, _ := s.Requests(ctx)
	if len(reqs) != 1 || !reqs[0].AlreadyAllowed {
		t.Fatalf("request should be flagged already allowed: %+v", reqs)
	}
	e, err := s.Approve(ctx, r.ID)
	if err != nil || e.Source != SourceManual {
		t.Fatalf("approve = %+v, %v; want the original manual entry", e, err)
	}
	if list, _ := s.List(ctx); len(list) != 1 {
		t.Fatalf("list = %v; want one entry", list)
	}
}

func TestReject(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	r, _, _ := s.Request(ctx, "dlddu/x", "sess-1", "a")
	if err := s.Reject(ctx, r.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := s.Reject(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reject twice err = %v", err)
	}
	if list, _ := s.List(ctx); len(list) != 0 {
		t.Fatalf("reject must not touch the allow list: %v", list)
	}
}
