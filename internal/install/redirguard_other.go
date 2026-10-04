//go:build !windows

package install

// redirectionGuard is a Windows mitigation: unknown elsewhere.
func redirectionGuard() *bool { return nil }
