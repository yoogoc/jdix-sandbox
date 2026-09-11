//go:build !linux

package safepath

import (
	"fmt"
	"os"
)

func OpenMount(root, relative string) (*os.File, error) {
	return nil, fmt.Errorf("pinned mounts require Linux openat2")
}
