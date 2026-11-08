//go:build linux

package evalruntime

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
)

// procSysKernel is where the AppArmor user-namespace switches live; tests point
// it at a fixture.
var procSysKernel = "/proc/sys/kernel"

// checkCallerIsolation accepts the dedicated bwrap at path only when the
// user-namespace grant behind it cannot be borrowed by another caller. On
// Ubuntu that grant is an AppArmor profile attached to the path, so two host
// settings must hold:
//
//   - kernel.apparmor_restrict_unprivileged_userns=1 and
//     kernel.apparmor_restrict_unprivileged_unconfined=1. With the second at 0 an
//     unprivileged process can aa-exec into any profile that grants userns -
//     LumenCortex's or one shipped by a distro package - so no per-path policy
//     isolates anything.
//   - Only a dedicated group may execute the file (mode 0750, group not root),
//     and this process is a member: a path-attached profile serves whoever can
//     execute the path, so execution itself is the caller check.
//
// What remains is documented, not hidden: every process that carries the group
// - this process and the host commands it starts - can run that bwrap.
func checkCallerIsolation(path string) error {
	for _, name := range []string{"apparmor_restrict_unprivileged_userns", "apparmor_restrict_unprivileged_unconfined"} {
		raw, err := os.ReadFile(procSysKernel + "/" + name)
		if err != nil {
			return fmt.Errorf("kernel.%s is unavailable (%v); this host cannot restrict user namespaces to LumenCortex", name, err)
		}
		if value := strings.TrimSpace(string(raw)); value != "1" {
			return fmt.Errorf("kernel.%s=%s; it must be 1, or any local process can obtain the user namespaces granted to %s", name, value, path)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("file ownership is unavailable")
	}
	if info.Mode().Perm()&0o001 != 0 {
		return fmt.Errorf("%s is executable by others (%v); restrict it to a dedicated group (0750)", path, info.Mode().Perm())
	}
	if stat.Gid == 0 {
		return fmt.Errorf("%s belongs to group root; use a dedicated group", path)
	}
	groups, err := os.Getgroups()
	if err != nil {
		return err
	}
	if uint32(os.Getegid()) != stat.Gid && !slices.Contains(groups, int(stat.Gid)) {
		return fmt.Errorf("this process is not in group %d, the only group that may execute %s", stat.Gid, path)
	}
	return nil
}
