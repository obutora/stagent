// Package version holds the stagent release and wire protocol versions.
package version

// Version is the stagent release version. The release script reads it to
// name the GitHub release (v<Version>) and the app pins the matching sha256
// sums.
const Version = "0.7.2"

// Protocol is the app ↔ bridge protocol version exchanged in `hello`.
// Bump it on any incompatible change to PROTOCOL.md.
const Protocol = 1

// FollowProtocol is the version of the `stagent follow` stream, sent in
// its hello frame. Bump it on any incompatible change to that section of
// PROTOCOL.md.
const FollowProtocol = 1
