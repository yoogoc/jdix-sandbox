//go:build !linux

package initd

import "errors"

// DropPrivileges is Linux-only; the capadmin tier does not exist elsewhere.
func DropPrivileges(uid, gid int) error {
	return errors.New("privilege dropping is only implemented on Linux")
}
