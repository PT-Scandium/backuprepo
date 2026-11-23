package syncer

import "time"

// localWins decides a conflict (both sides changed since the last sync) under
// the owner's "newest wins" rule: true keeps the local version (it is
// uploaded), false keeps the remote one (it is downloaded and the local file
// is moved to the conflicts folder, so neither choice loses data).
//
// local is the local file's modification time. remote is the remote version's
// source modification time when its uploader recorded one (bb does from
// v1.0.13 on; rclone and the Backblaze web UI do too), otherwise its upload
// time — which is always later than the edit it carries.
//
// TODO(edisonch): refine the policy. Things to weigh:
//   - Clock skew: this PC's clock vs the other uploader's; a few seconds of
//     difference can pick the "wrong" newest. A tolerance window could treat
//     near-equal times as a tie.
//   - Ties: which side should win when the times are (nearly) equal? Keeping
//     the remote moves the local copy aside (nothing lost locally); keeping
//     the local adds a new locked version in the bucket.
//   - A zero time (unknown) on either side.
func localWins(local, remote time.Time) bool {
	return local.After(remote)
}
