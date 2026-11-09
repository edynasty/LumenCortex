//go:build linux

package evalruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"golang.org/x/sys/unix"
)

// ServeBroker accepts only fixed Eval operations. Root authority stays behind
// administrator policy and a PID/starttime-bound one-use connection grant.
func ServeBroker(ctx context.Context, policy BrokerPolicy) error {
	if os.Geteuid() != 0 || policy.WorkerUID <= 0 || policy.WorkerGID <= 0 {
		return errors.New("broker requires root and a distinct non-root worker UID/GID")
	}
	for _, path := range []string{policy.Root, policy.Grants, filepath.Dir(policy.Socket)} {
		if err := brokerTrustedDir(path); err != nil {
			return err
		}
	}
	for _, path := range []string{policy.Bubblewrap, "/usr/bin/setpriv", policy.Python, policy.JavaScript} {
		if err := checkTrustedExecutable(path); err != nil {
			return err
		}
	}
	info, err := os.Stat(policy.Bubblewrap)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("broker bwrap must be root-only (0700), not a reusable group grant")
	}
	// A crash leaves the socket pathname. A process-held lock distinguishes it
	// from a live broker without connecting to (and occupying) its client slot.
	lockFD, err := unix.Open(policy.Socket+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(lockFD)
	if err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("broker already running: %w", err)
	}
	if stale, e := os.Lstat(policy.Socket); e == nil {
		if stale.Mode()&os.ModeSocket == 0 {
			return errors.New("broker socket path is not a socket")
		}
		st, ok := stale.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			return errors.New("stale broker socket is not root-owned")
		}
		if e = os.Remove(policy.Socket); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: policy.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	// Directory permits the trusted Core client to connect; actual authority is
	// the root-issued peer-specific grant, not socket write permission.
	if err = os.Chmod(policy.Socket, 0666); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, 1)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer conn.Close()
			_ = serveBrokerConnection(ctx, conn, policy)
		}()
	}
}

func brokerTrustedDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("broker directories must be clean absolute paths")
	}
	for dir := path; ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("broker directory is not a real directory")
		}
		if err = rootOwnedNotShared(dir, info); err != nil {
			return err
		}
		if dir == "/" {
			return nil
		}
	}
}

func serveBrokerConnection(parent context.Context, conn *net.UnixConn, policy BrokerPolicy) error {
	peer, err := brokerPeer(conn)
	if err != nil {
		return err
	}
	_, _, start, err := procStat(int(peer.Pid))
	if err != nil {
		return err
	}
	pidfd, err := unix.PidfdOpen(int(peer.Pid), 0)
	if err != nil {
		return err
	}
	defer unix.Close(pidfd)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// Read exactly the small hello, without consuming any following RPC bytes.
	var hello []byte
	var b [1]byte
	for len(hello) < 256 {
		if _, err = io.ReadFull(conn, b[:]); err != nil {
			return err
		}
		if b[0] == '\n' {
			break
		}
		hello = append(hello, b[0])
	}
	grantID := string(hello)
	if len(grantID) != 32 {
		return errors.New("invalid grant ID")
	}
	for _, c := range grantID {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("invalid grant ID")
		}
	}
	grantPath := filepath.Join(policy.Grants, grantID+".json")
	info, err := os.Lstat(grantPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("grant must be root-only regular file")
	}
	if err = rootOwnedNotShared(grantPath, info); err != nil {
		return err
	}
	data, err := os.ReadFile(grantPath)
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return errors.New("oversized grant")
	}
	var grant BrokerGrant
	if err = json.Unmarshal(data, &grant); err != nil {
		return err
	}
	if grant.SchemaVersion != BrokerSchemaVersion || grant.ID != grantID || grant.UID != int(peer.Uid) || grant.GID != int(peer.Gid) || grant.PID != int(peer.Pid) || grant.Starttime != start || !grant.ExpiresAt.After(time.Now()) || !hostIdentityAlive(grant.PID, start) {
		return errors.New("grant does not authorize this live caller")
	}
	if err := grant.Boundary.Validate(); err != nil {
		return err
	}
	activePath := grantPath + ".consumed"
	if err = os.Link(grantPath, activePath); err != nil {
		return fmt.Errorf("grant already consumed: %w", err)
	}
	if err = os.Remove(grantPath); err != nil {
		return err
	}
	// Consumed files remain replay tombstones; deletion is grant revocation.
	_ = conn.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	closeOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer closeOnCancel()
	root, err := os.MkdirTemp(policy.Root, "connection-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	var rpc *rpcConn
	ready := make(chan struct{})
	m, err := New(Options{Root: filepath.Join(root, "manager"), Python: policy.Python, JavaScript: policy.JavaScript, Budget: policy.Budget, Boundary: &grant.Boundary,
		Sandbox: SandboxOptions{BubblewrapPath: policy.Bubblewrap}, identity: &brokerIdentity{uid: policy.WorkerUID, gid: policy.WorkerGID},
		OnEvent: func(e evalapi.Event) {
			select {
			case <-ready:
				_ = rpc.notifyPeer("event", e)
			default:
			}
		},
	})
	if err != nil {
		return err
	}
	defer m.Dispose(context.Background())
	// Monitor revocation and peer death even during an in-flight Cell.
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, e := os.Stat(activePath)
				if e != nil || time.Now().After(grant.ExpiresAt) || unix.PidfdSendSignal(pidfd, 0, nil, 0) != nil {
					cancel()
					return
				}
			}
		}
	}()
	var mu sync.Mutex
	seen := make(map[uint64]struct{})
	active := make(map[uint64]context.CancelFunc)
	cancelled := make(map[uint64]bool)
	rpc = newRPCConn(conn, conn, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method != "request" {
			return packBrokerReply(nil, evalapi.ErrDenied), nil
		}
		var call brokerCall
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&call); err != nil {
			return packBrokerReply(nil, err), nil
		}
		mu.Lock()
		_, duplicate := seen[call.Nonce]
		if duplicate || call.Nonce == 0 || len(seen) >= 65536 {
			mu.Unlock()
			return packBrokerReply(nil, errors.New("broker nonce replay or request limit")), nil
		}
		seen[call.Nonce] = struct{}{}
		mu.Unlock()
		if call.SchemaVersion != BrokerSchemaVersion || call.Deadline.IsZero() || !call.Deadline.After(time.Now()) {
			return packBrokerReply(nil, evalapi.ErrExpired), nil
		}
		if call.Deadline.After(grant.ExpiresAt) {
			call.Deadline = grant.ExpiresAt
		}
		opCtx, stop := context.WithDeadline(ctx, call.Deadline)
		defer stop()
		mu.Lock()
		active[call.Nonce] = stop
		if cancelled[call.Nonce] {
			stop()
			delete(cancelled, call.Nonce)
		}
		mu.Unlock()
		defer func() { mu.Lock(); delete(active, call.Nonce); mu.Unlock() }()
		value, e := dispatchBroker(opCtx, m, rpc, root, call)
		return packBrokerReply(value, e), nil
	}, func(method string, raw json.RawMessage) {
		if method != "cancel" {
			return
		}
		var nonce uint64
		if json.Unmarshal(raw, &nonce) != nil {
			return
		}
		mu.Lock()
		stop := active[nonce]
		if stop == nil && len(cancelled) < 65536 {
			cancelled[nonce] = true
		}
		mu.Unlock()
		if stop != nil {
			stop()
		}
	})
	close(ready)
	rpc.limit = 16 << 20
	if _, err = conn.Write([]byte("OK\n")); err != nil {
		return err
	}
	rpc.start()
	select {
	case <-ctx.Done():
	case <-rpc.done:
	}
	cancel()
	rpc.close(errConnClosed)
	return m.Dispose(context.Background())
}

func brokerPeer(conn *net.UnixConn) (*unix.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var peer *unix.Ucred
	var inner error
	err = raw.Control(func(fd uintptr) { peer, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	return peer, errors.Join(err, inner)
}
func hostIdentityAlive(pid int, start uint64) bool {
	return (hostIdentity{pid: pid, start: start}).alive()
}

func dispatchBroker(ctx context.Context, m *Manager, rpc *rpcConn, root string, c brokerCall) (any, error) {
	switch c.Method {
	case "discover":
		return m.Discover(ctx)
	case "create":
		if c.Owner == "" || len(c.Owner) > 180 {
			return nil, evalapi.ErrInvalid
		}
		if c.Permission != "" && c.Permission != "workspace" {
			return nil, evalapi.ErrDenied
		}
		if err := validateBrokerBudget(c.Budget, m.defaultBudget); err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp(root, "workspace-")
		if err != nil {
			return nil, err
		}
		return m.Create(ctx, evalapi.CreateOptions{RuntimeID: c.Runtime, OwnerID: c.Owner, AgentID: c.Owner, ParentAgentID: c.Parent, Workspace: dir, Permission: "workspace", Budget: c.Budget, LeaseUntil: c.LeaseUntil, Label: c.Label})
	case "sessions":
		return m.Sessions(ctx, c.Owner)
	case "session":
		return m.Session(ctx, c.Session, c.Owner)
	case "health":
		return m.Health(ctx, c.Session, c.Owner)
	case "dispose":
		return nil, m.Dispose(ctx)
	case "execute":
		if c.Request == nil {
			return nil, evalapi.ErrInvalid
		}
		if c.Depth < 0 {
			return nil, evalapi.ErrInvalid
		}
		if err := validateBrokerBudget(c.Request.Budget, m.defaultBudget); err != nil {
			return nil, err
		}
		var caller evalapi.ToolCaller
		if c.Callbacks {
			caller = brokerCaller{conn: rpc, requestID: c.Request.ID}
		}
		return m.Execute(evalapi.WithCallDepth(ctx, c.Depth), *c.Request, caller)
	case "interrupt":
		if c.Target == nil {
			return nil, evalapi.ErrInvalid
		}
		return m.InterruptExecution(ctx, c.Session, c.Owner, *c.Target)
	case "reset":
		return nil, m.Reset(ctx, c.Session, c.Owner)
	case "close":
		return nil, m.Close(ctx, c.Session, c.Owner)
	case "grant":
		if c.Grant == nil {
			return nil, evalapi.ErrInvalid
		}
		return nil, m.Grant(ctx, c.Session, c.Owner, *c.Grant)
	case "revoke":
		return nil, m.Revoke(ctx, c.Session, c.Owner, c.Agent)
	case "results":
		return m.Results(ctx, c.Session, c.Owner)
	case "artifact":
		meta, data, err := m.Artifact(ctx, c.Session, c.Owner, c.ArtifactID)
		return brokerArtifact{meta, data}, err
	case "put":
		return m.PutArtifact(ctx, c.Session, c.Owner, c.Name, c.MIME, c.Data)
	case "closeOwner":
		return nil, m.CloseOwner(ctx, c.Owner)
	case "forgetOwner":
		return nil, m.ForgetOwner(ctx, c.Owner)
	case "revokeAgent":
		return nil, m.RevokeAgent(ctx, c.Agent)
	case "boundary":
		if c.Binding == nil {
			return nil, evalapi.ErrInvalid
		}
		return m.AssertBoundary(ctx, c.Session, c.Owner, *c.Binding)
	default:
		return nil, fmt.Errorf("broker operation denied: %s", strconv.Quote(c.Method))
	}
}
