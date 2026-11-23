package syncer

import (
	"testing"
	"time"
)

// TestDecide covers every combination of snapshot, local and remote state.
// Rules: one-sided changes copy over; both changed → newer wins; same bytes →
// just record; never delete (a missing side is restored); gone on both → forget.
func TestDecide(t *testing.T) {
	older := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	for _, tc := range []struct {
		name string
		in   Input
		want Action
	}{
		// No snapshot (first sync of this file).
		{"new local only", Input{LocalExists: true}, Upload},
		{"new remote only", Input{RemoteExists: true}, Download},
		{"nothing anywhere", Input{}, None},
		{"both, same bytes", Input{LocalExists: true, RemoteExists: true, SameContent: true}, Record},
		{"both differ, local newer", Input{LocalExists: true, RemoteExists: true, LocalTime: newer, RemoteTime: older}, KeepLocal},
		{"both differ, remote newer", Input{LocalExists: true, RemoteExists: true, LocalTime: older, RemoteTime: newer}, KeepRemote},
		// With a snapshot.
		{"unchanged both", Input{HasBase: true, LocalExists: true, RemoteExists: true, SameContent: true}, None},
		{"unchanged, bytes not compared", Input{HasBase: true, LocalExists: true, RemoteExists: true, LocalTime: older, RemoteTime: newer}, None},
		{"local edit", Input{HasBase: true, LocalExists: true, RemoteExists: true, LocalChanged: true}, Upload},
		{"remote edit", Input{HasBase: true, LocalExists: true, RemoteExists: true, RemoteChanged: true}, Download},
		{"both edited, local newer", Input{HasBase: true, LocalExists: true, RemoteExists: true, LocalChanged: true, RemoteChanged: true, LocalTime: newer, RemoteTime: older}, KeepLocal},
		{"both edited, remote newer", Input{HasBase: true, LocalExists: true, RemoteExists: true, LocalChanged: true, RemoteChanged: true, LocalTime: older, RemoteTime: newer}, KeepRemote},
		{"both edited to the same bytes", Input{HasBase: true, LocalExists: true, RemoteExists: true, LocalChanged: true, RemoteChanged: true, SameContent: true}, Record},
		{"deleted locally → restore", Input{HasBase: true, RemoteExists: true, LocalChanged: true}, Download},
		{"deleted locally, remote edited → restore newer", Input{HasBase: true, RemoteExists: true, LocalChanged: true, RemoteChanged: true}, Download},
		{"deleted remotely → restore", Input{HasBase: true, LocalExists: true, RemoteChanged: true}, Upload},
		{"deleted remotely, local edited → upload", Input{HasBase: true, LocalExists: true, LocalChanged: true, RemoteChanged: true}, Upload},
		{"deleted on both sides", Input{HasBase: true, LocalChanged: true, RemoteChanged: true}, Forget},
	} {
		if got := Decide(tc.in); got != tc.want {
			t.Errorf("%s: Decide = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestActionString: every action has a readable name for dry-run output.
func TestActionString(t *testing.T) {
	for a := None; a <= Forget; a++ {
		if s := a.String(); s == "" || s == "unknown" {
			t.Errorf("Action(%d).String() = %q", a, s)
		}
	}
	if Action(99).String() != "unknown" {
		t.Error("out-of-range action should be unknown")
	}
}
