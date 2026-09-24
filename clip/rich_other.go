//go:build !darwin && !windows && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package clip

// Anywhere else there is no known way to set an HTML flavor, so Write falls
// straight through to plain text.
func platformWriteRich(Content) error { return errNoRich }
