// Package syncer keeps a local folder and a bucket folder (or the whole
// bucket) in two-way sync (ADR-028). For every file it compares both sides
// with the snapshot recorded at the last sync, decides with Decide — a pure
// function — and then uploads, downloads or just records. Its rules: a change
// on one side is copied to the other; when both sides changed, the newer
// version wins and a losing local edit is moved aside, never discarded; and
// nothing is ever deleted — a file missing on one side is restored from the
// other.
package syncer

import "time"

// Action is what sync does for one file.
type Action int

const (
	// None: both sides already match the snapshot.
	None Action = iota
	// Record: both sides hold the same bytes; only the snapshot is refreshed.
	Record
	// Upload: the local file is new or changed (or the remote copy is gone).
	Upload
	// Download: the remote file is new or changed (or the local copy is gone).
	Download
	// KeepLocal: both changed and the local version is newer; upload it.
	KeepLocal
	// KeepRemote: both changed and the remote version is newer; move the local
	// file aside (to the conflicts folder), then download.
	KeepRemote
	// Forget: the file is gone on both sides; drop its snapshot.
	Forget
)

// String names the action for dry-run output and logs.
func (a Action) String() string {
	switch a {
	case None:
		return "unchanged"
	case Record:
		return "same on both sides"
	case Upload:
		return "upload"
	case Download:
		return "download"
	case KeepLocal:
		return "conflict: upload newer local"
	case KeepRemote:
		return "conflict: download newer remote, local kept aside"
	case Forget:
		return "gone on both sides"
	}
	return "unknown"
}

// Input is everything Decide needs about one file. Changed flags are relative
// to the snapshot and ignored when there is none (HasBase false).
type Input struct {
	HasBase       bool
	LocalExists   bool
	RemoteExists  bool
	LocalChanged  bool
	RemoteChanged bool
	// SameContent: both exist and hold identical bytes (equal SHA-1).
	SameContent bool
	// LocalTime is the local file's modification time; RemoteTime the time
	// recorded for the remote version (its source modification time when the
	// uploader stored one, else its upload time).
	LocalTime  time.Time
	RemoteTime time.Time
}

// Decide returns the action for one file. It never deletes: a file missing on
// one side is restored from the other, whatever the snapshot says.
func Decide(in Input) Action {
	switch {
	case !in.LocalExists && !in.RemoteExists:
		if in.HasBase {
			return Forget
		}
		return None
	case in.LocalExists && in.RemoteExists && in.HasBase && !in.LocalChanged && !in.RemoteChanged:
		return None // both still match the snapshot (bytes need not be compared)
	case in.LocalExists && in.RemoteExists && in.SameContent:
		return Record
	case !in.RemoteExists:
		return Upload // new locally, or deleted remotely: never delete
	case !in.LocalExists:
		return Download // new remotely, or deleted locally: never delete
	}
	// Both exist with different content.
	if in.HasBase {
		switch {
		case in.LocalChanged && !in.RemoteChanged:
			return Upload
		case in.RemoteChanged && !in.LocalChanged:
			return Download
		}
		// Both changed: newest wins.
	}
	if localWins(in.LocalTime, in.RemoteTime) {
		return KeepLocal
	}
	return KeepRemote
}
