package isolation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"jdix.io/sandbox/pkg/bwrap"
	"jdix.io/sandbox/pkg/safepath"
)

// DetectFilesystem exercises the actual setup and workload filters and every
// configured volume. An unsuccessful probe never advertises a weaker mode.
func DetectFilesystem(ctx context.Context, binary string, volumes map[string]string) Result {
	r := Result{Probes: map[string]string{}}
	fail := func(err error) Result { r.Reason = err.Error(); r.Probes["filesystem"] = err.Error(); return r }
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	args := []string{"--unshare-user", "--uid", "1000", "--gid", "1000",
		"--cap-drop", "ALL", "--ro-bind", "/proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--ro-bind", exe, "/jdix-probe", "--clearenv", "--die-with-parent", "--new-session"}
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	names := make([]string, 0, len(volumes))
	for n := range volumes {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, name := range names {
		f, err := safepath.OpenMount(volumes[name], "")
		if err != nil {
			return fail(fmt.Errorf("volume %s: %w", name, err))
		}
		args = append(args, "--ro-bind-fd", strconv.Itoa(3+len(files)), fmt.Sprintf("/volume-%d", i))
		files = append(files, f)
	}
	filter, err := WorkloadFilter()
	if err != nil {
		return fail(err)
	}
	args = append(args, "--seccomp", strconv.Itoa(3+len(files)))
	files = append(files, filter)
	args = append(args, "--", "/jdix-probe", "--filesystem-probe-child")
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.ExtraFiles, cmd.Env = files, []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fail(fmt.Errorf("filesystem probe: %s", trim(out, err)))
	}
	if strings.TrimSpace(string(out)) != "filesystem-ok" {
		return fail(fmt.Errorf("unexpected filesystem probe output: %s", out))
	}
	r.Tier, r.Reason = bwrap.TierFilesystem, "pinned volume mounts and protected proc view available"
	r.Probes["filesystem"] = "ok"
	return r
}

// CheckFilesystemView runs inside the completed view, including in the readiness
// probe. A readable root/cwd/fd in a different mount namespace would expose an
// outer process's broader filesystem, so that configuration is rejected.
func CheckFilesystemView() error {
	own, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		base := "/proc/" + e.Name()
		tasks, _ := os.ReadDir(base + "/task")
		paths := []string{base}
		for _, t := range tasks {
			paths = append(paths, base+"/task/"+t.Name())
		}
		for _, p := range paths {
			ns, nerr := os.Readlink(p + "/ns/mnt")
			if nerr == nil && ns == own {
				continue
			}
			for _, link := range []string{"root", "cwd"} {
				if _, err := os.Readlink(p + "/" + link); err == nil {
					return fmt.Errorf("outer process filesystem is reachable through %s/%s", p, link)
				}
			}
			if f, err := os.Open(p + "/environ"); err == nil {
				f.Close()
				return fmt.Errorf("outer process environment is readable: %s", p)
			}
			if fds, err := os.ReadDir(p + "/fd"); err == nil {
				for _, fd := range fds {
					if _, err := os.Readlink(p + "/fd/" + fd.Name()); err == nil {
						return fmt.Errorf("outer process descriptor is accessible: %s/fd/%s", p, fd.Name())
					}
				}
			}
		}
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(status), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "CapEff:", "CapPrm:", "CapAmb:":
			if f[1] != "0000000000000000" {
				return fmt.Errorf("workload retains %s %s", f[0], f[1])
			}
		case "NoNewPrivs:":
			if f[1] != "1" {
				return fmt.Errorf("no_new_privs is not set")
			}
		case "Seccomp:":
			if f[1] != "2" {
				return fmt.Errorf("seccomp is not active")
			}
		}
	}
	// A source FD retained by bwrap would remain reachable after pivot_root.
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, e := range fds {
		n, _ := strconv.Atoi(e.Name())
		if n < 3 {
			continue
		}
		p := filepath.Join("/proc/self/fd", e.Name())
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return fmt.Errorf("directory FD leaked into workload: %s", p)
		}
	}
	return nil
}
