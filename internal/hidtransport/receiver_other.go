//go:build !darwin

package hidtransport

// configureOpenMode is a no-op off macOS: only hidapi's Darwin backend defaults
// to exclusive (seize) open, and only it exposes SetOpenExclusive.
func configureOpenMode() {}
