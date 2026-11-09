package evalruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

type brokerCallback struct {
	ctx    context.Context
	caller evalapi.ToolCaller
}
type BrokerClient struct {
	socket    *net.UnixConn
	rpc       *rpcConn
	seq       atomic.Uint64
	mu        sync.Mutex
	callbacks map[string]brokerCallback
	closeOnce sync.Once
	closeErr  error
}

var _ Host = (*BrokerClient)(nil)

func DialBroker(ctx context.Context, opts BrokerOptions, onEvent func(evalapi.Event)) (*BrokerClient, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", opts.Socket)
	if err != nil {
		return nil, err
	}
	socket := conn.(*net.UnixConn)
	if err = verifyBrokerPeer(socket); err != nil {
		socket.Close()
		return nil, err
	}
	_ = socket.SetDeadline(time.Now().Add(5 * time.Second))
	if len(opts.GrantID) != 32 {
		socket.Close()
		return nil, errors.New("broker grant ID must be 32 hex characters")
	}
	if _, err = io.WriteString(socket, opts.GrantID+"\n"); err != nil {
		socket.Close()
		return nil, err
	}
	var hello [3]byte
	if _, err = io.ReadFull(socket, hello[:]); err != nil || string(hello[:]) != "OK\n" {
		socket.Close()
		return nil, errors.New("broker refused caller authorization")
	}
	_ = socket.SetDeadline(time.Time{})
	b := &BrokerClient{socket: socket, callbacks: make(map[string]brokerCallback)}
	b.rpc = newRPCConn(socket, socket, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		var call brokerToolCall
		if err := json.Unmarshal(raw, &call); err != nil {
			return packBrokerReply(nil, err), nil
		}
		b.mu.Lock()
		entry, ok := b.callbacks[call.RequestID]
		b.mu.Unlock()
		if !ok || entry.caller == nil || entry.ctx.Err() != nil {
			return packBrokerReply(nil, evalapi.ErrDenied), nil
		}
		switch method {
		case "tools":
			v, e := entry.caller.Tools(entry.ctx)
			return packBrokerReply(v, e), nil
		case "tool":
			v, e := entry.caller.Call(evalapi.WithCallDepth(entry.ctx, evalapi.CallDepth(entry.ctx)+1), call.Name, call.Arguments)
			return packBrokerReply(v, e), nil
		default:
			return packBrokerReply(nil, evalapi.ErrDenied), nil
		}
	}, func(method string, raw json.RawMessage) {
		if method == "event" && onEvent != nil {
			var e evalapi.Event
			if json.Unmarshal(raw, &e) == nil {
				onEvent(e)
			}
		}
	})
	b.rpc.limit = 16 << 20
	b.rpc.start()
	return b, nil
}

func (b *BrokerClient) call(ctx context.Context, c brokerCall, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.SchemaVersion = BrokerSchemaVersion
	c.Nonce = b.seq.Add(1)
	c.Deadline = time.Now().Add(5 * time.Minute)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(c.Deadline) {
		c.Deadline = deadline
	}
	raw, err := b.rpc.call(ctx, "request", c)
	if err != nil {
		if ctx.Err() != nil {
			_ = b.rpc.notifyPeer("cancel", c.Nonce)
		}
		return err
	}
	return decodeBrokerReply(raw, out)
}
func (b *BrokerClient) Register(evalapi.Manifest) error {
	return errors.New("broker runtime registry is administrator-owned")
}
func (b *BrokerClient) Discover(ctx context.Context) (out []evalapi.Manifest, err error) {
	err = b.call(ctx, brokerCall{Method: "discover"}, &out)
	return
}
func (b *BrokerClient) Create(ctx context.Context, o evalapi.CreateOptions) (out evalapi.Session, err error) {
	if o.AgentID != "" && o.AgentID != o.OwnerID {
		return out, evalapi.ErrDenied
	}
	err = b.call(ctx, brokerCall{Method: "create", Owner: o.OwnerID, Runtime: o.RuntimeID, Parent: o.ParentAgentID, Permission: o.Permission, Budget: o.Budget, LeaseUntil: o.LeaseUntil, Label: o.Label}, &out)
	return
}
func (b *BrokerClient) Sessions(ctx context.Context, o string) (out []evalapi.Session, err error) {
	err = b.call(ctx, brokerCall{Method: "sessions", Owner: o}, &out)
	return
}
func (b *BrokerClient) Session(ctx context.Context, s, o string) (out evalapi.Session, err error) {
	err = b.call(ctx, brokerCall{Method: "session", Session: s, Owner: o}, &out)
	return
}
func (b *BrokerClient) Health(ctx context.Context, s, o string) (out evalapi.Health, err error) {
	err = b.call(ctx, brokerCall{Method: "health", Session: s, Owner: o}, &out)
	return
}
func (b *BrokerClient) Execute(ctx context.Context, r evalapi.Request, caller evalapi.ToolCaller) (out evalapi.Result, err error) {
	b.mu.Lock()
	if _, ok := b.callbacks[r.ID]; ok {
		b.mu.Unlock()
		return out, errors.New("duplicate active broker request")
	}
	b.callbacks[r.ID] = brokerCallback{ctx, caller}
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.callbacks, r.ID); b.mu.Unlock() }()
	err = b.call(ctx, brokerCall{Method: "execute", Request: &r, Callbacks: caller != nil, Depth: evalapi.CallDepth(ctx)}, &out)
	return
}
func (b *BrokerClient) InterruptExecution(ctx context.Context, s, o string, t evalapi.InterruptTarget) (out evalapi.InterruptOutcome, err error) {
	err = b.call(ctx, brokerCall{Method: "interrupt", Session: s, Owner: o, Target: &t}, &out)
	return
}
func (b *BrokerClient) Reset(ctx context.Context, s, o string) error {
	return b.call(ctx, brokerCall{Method: "reset", Session: s, Owner: o}, nil)
}
func (b *BrokerClient) Close(ctx context.Context, s, o string) error {
	return b.call(ctx, brokerCall{Method: "close", Session: s, Owner: o}, nil)
}
func (b *BrokerClient) Grant(ctx context.Context, s, o string, g evalapi.Grant) error {
	return b.call(ctx, brokerCall{Method: "grant", Session: s, Owner: o, Grant: &g}, nil)
}
func (b *BrokerClient) Revoke(ctx context.Context, s, o, a string) error {
	return b.call(ctx, brokerCall{Method: "revoke", Session: s, Owner: o, Agent: a}, nil)
}
func (b *BrokerClient) Results(ctx context.Context, s, o string) (out []evalapi.Result, err error) {
	err = b.call(ctx, brokerCall{Method: "results", Session: s, Owner: o}, &out)
	return
}
func (b *BrokerClient) Artifact(ctx context.Context, s, o, id string) (evalapi.Artifact, []byte, error) {
	var out brokerArtifact
	err := b.call(ctx, brokerCall{Method: "artifact", Session: s, Owner: o, ArtifactID: id}, &out)
	return out.Meta, out.Data, err
}
func (b *BrokerClient) PutArtifact(ctx context.Context, s, o, name, mime string, data []byte) (out evalapi.Artifact, err error) {
	err = b.call(ctx, brokerCall{Method: "put", Session: s, Owner: o, Name: name, MIME: mime, Data: data}, &out)
	return
}
func (b *BrokerClient) CloseOwner(ctx context.Context, o string) error {
	return b.call(ctx, brokerCall{Method: "closeOwner", Owner: o}, nil)
}
func (b *BrokerClient) ForgetOwner(ctx context.Context, o string) error {
	return b.call(ctx, brokerCall{Method: "forgetOwner", Owner: o}, nil)
}
func (b *BrokerClient) RevokeAgent(ctx context.Context, a string) error {
	return b.call(ctx, brokerCall{Method: "revokeAgent", Agent: a}, nil)
}
func (b *BrokerClient) AssertBoundary(ctx context.Context, s, o string, binding evalapi.BoundaryContext) (out evalapi.BoundaryReport, err error) {
	err = b.call(ctx, brokerCall{Method: "boundary", Session: s, Owner: o, Binding: &binding}, &out)
	return
}
func (b *BrokerClient) Dispose(ctx context.Context) error {
	b.closeOnce.Do(func() {
		b.closeErr = b.call(ctx, brokerCall{Method: "dispose"}, nil)
		b.rpc.close(errConnClosed)
		b.closeErr = errors.Join(b.closeErr, b.socket.Close())
	})
	return b.closeErr
}
