//go:build !darwin

package bootstrap

// Swap does nothing off macOS.
func Swap() Result { return Result{} }
