//go:build !linux

package isolation

// hasChroot is Linux-only in any meaningful sense; on other platforms the
// detector has already short-circuited to the chroot tier.
func hasChroot() bool { return false }
