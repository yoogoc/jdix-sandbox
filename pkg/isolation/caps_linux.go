//go:build linux

package isolation

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// capSysChroot is CAP_SYS_CHROOT's bit position in the capability bitmask.
const capSysChroot = 18

// hasChroot reads the effective capability set rather than attempting a real
// chroot, which would need a fork and a throwaway directory.
func hasChroot() bool {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		hex := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		v, err := strconv.ParseUint(hex, 16, 64)
		if err != nil {
			return false
		}
		return v&(1<<capSysChroot) != 0
	}
	return false
}
