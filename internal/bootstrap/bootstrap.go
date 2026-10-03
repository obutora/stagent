// Package bootstrap moves stagent's long-lived processes on macOS from the
// bootstrap namespace of the login session they were started in to the
// user's per-user one, as tmux does (compat/daemon-darwin.c).
//
// A process started from a terminal of the GUI session inherits that
// session's bootstrap port. After the user logs out of the GUI the process
// keeps running, but the port no longer reaches Mach services such as
// configd, so programs it starts later cannot resolve host names (curl, git,
// MCP servers). The per-user bootstrap port outlives the GUI session, and
// while the user is logged in it still reaches the keychain.
package bootstrap

// Result is what Swap did.
type Result struct {
	// Applies: the host is macOS, where Swap makes the swap.
	Applies bool
	// Swapped: the process uses the per-user bootstrap port now.
	Swapped bool
	// Err says why the swap failed; "" when it did not.
	Err string
}

// SurvivesLogout reports whether the process can still reach Mach services
// (DNS) after the user logs out of the GUI; known is false off macOS.
func (r Result) SurvivesLogout() (survives, known bool) {
	return r.Swapped, r.Applies
}
