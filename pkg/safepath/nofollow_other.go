//go:build !linux

package safepath

import "syscall"

const syscallNoFollow = syscall.O_NOFOLLOW
