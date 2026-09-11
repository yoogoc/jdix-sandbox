//go:build !linux

package isolation

import (
	"fmt"
	"os"
)

func ProtectSupervisor() error          { return fmt.Errorf("filesystem isolation requires Linux") }
func BecomeSubreaper() error            { return fmt.Errorf("subreaper requires Linux") }
func WorkloadFilter() (*os.File, error) { return nil, fmt.Errorf("seccomp requires Linux") }
