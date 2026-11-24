package session_test

import (
	"errors"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-N11: a request that does not mention lanes is about the person's chat, and
// anything that is not a lane is refused rather than defaulted — a misspelt
// lane silently reading the chat would be a client that thinks it is watching
// the monitor.
func TestNormalizeLane(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    session.Lane
		wantErr bool
	}{
		{raw: "", want: session.LaneParent},
		{raw: "parent", want: session.LaneParent},
		{raw: "monitor", want: session.LaneMonitor},
		{raw: "Monitor", wantErr: true},
		{raw: "PARENT", wantErr: true},
		{raw: "monito", wantErr: true},
		{raw: "worker", wantErr: true},
		{raw: "parent,monitor", wantErr: true},
	} {
		got, err := session.NormalizeLane(tc.raw)
		if tc.wantErr {
			if !errors.Is(err, session.ErrInvalidInput) {
				t.Fatalf("NormalizeLane(%q) = %q, %v; want ErrInvalidInput", tc.raw, got, err)
			}
			if got != "" {
				t.Fatalf("NormalizeLane(%q) refused but still returned %q", tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("NormalizeLane(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

// IsDefault is what decides whether a call takes the path it took before the
// pair existed, so the empty lane and the parent lane have to agree.
func TestLaneIsDefault(t *testing.T) {
	if !session.Lane("").IsDefault() || !session.LaneParent.IsDefault() {
		t.Fatal("the empty lane and the parent lane are both the conversation a lane-unaware client reaches")
	}
	if session.LaneMonitor.IsDefault() {
		t.Fatal("the monitor lane must never be what a lane-unaware client reaches")
	}
	if len(session.Lanes) != 2 {
		t.Fatalf("Lanes = %v, want exactly the two conversations of a manager", session.Lanes)
	}
}
