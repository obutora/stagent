//go:build !linux && !darwin

package bootid

func get() string { return "" }
