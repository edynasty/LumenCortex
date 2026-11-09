//go:build linux

package evalruntime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

type brokerIdentity struct{ uid, gid int }

func prepareBrokerLaunch(identity *brokerIdentity) ([]*os.File, func(), error) {
	if identity == nil {
		return nil, func() {}, nil
	}
	if os.Geteuid() != 0 || identity.uid <= 0 || identity.gid <= 0 {
		return nil, nil, errors.New("broker requires root launcher and non-root worker identity")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	holder := exec.Command("/bin/cat")
	holder.Stdin = r
	holder.Env = []string{"PATH=/usr/bin:/bin"}
	holder.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: 1}, {ContainerID: identity.uid, HostID: identity.uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: 1}, {ContainerID: identity.gid, HostID: identity.gid, Size: 1}}, GidMappingsEnableSetgroups: true, Pdeathsig: syscall.SIGKILL}
	if err = holder.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, nil, err
	}
	r.Close()
	var files []*os.File
	release := func() {
		for _, f := range files {
			_ = f.Close()
		}
		_ = w.Close()
		_ = holder.Wait()
	}
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/user", holder.Process.Pid))
	if err != nil {
		release()
		return nil, nil, err
	}
	files = append(files, ns)
	seccomp, err := brokerSeccomp()
	if err != nil {
		release()
		return nil, nil, err
	}
	files = append(files, seccomp)
	return files, release, nil
}

func brokerSeccomp() (*os.File, error) {
	arch := uint32(0)
	switch runtime.GOARCH {
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	default:
		return nil, errors.New("broker seccomp architecture unsupported")
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	for _, nr := range []uint32{unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_BPF, unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY, unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_SETNS, unix.SYS_UNSHARE, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_REBOOT, unix.SYS_KEXEC_LOAD, unix.SYS_PERF_EVENT_OPEN} {
		filters = append(filters, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: nr, Jf: 1}, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	}
	// Return ENOSYS for clone3 so libc can use ordinary clone. Namespace flags
	// on clone itself are rejected while threads and subprocesses remain usable.
	filters = append(filters,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CLONE3, Jf: 1},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CLONE, Jf: 3},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP, Jf: 1},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	fd, err := unix.MemfdCreate("lc-seccomp", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "lc-seccomp")
	if err = binary.Write(f, binary.LittleEndian, filters); err == nil {
		_, err = f.Seek(0, 0)
	}
	if err == nil {
		_, err = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
