//go:build !darwin && !windows

package powermon

// Start is a no-op on platforms without a power-event source wired up.
func Start() {}
