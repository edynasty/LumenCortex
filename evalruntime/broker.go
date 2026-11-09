package evalruntime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

const BrokerSchemaVersion = 1

// BrokerOptions names only an administrator-installed socket and a one-use
// process grant. It grants no executable, mount or environment authority.
type BrokerOptions struct {
	Socket  string `json:"socket"`
	GrantID string `json:"grantId"`
}

// BrokerPolicy is loaded by the privileged service, never from its socket.
type BrokerPolicy struct {
	Socket     string         `json:"socket"`
	Root       string         `json:"root"`
	Grants     string         `json:"grants"`
	Python     string         `json:"python"`
	JavaScript string         `json:"javascript"`
	Bubblewrap string         `json:"bubblewrap"`
	WorkerUID  int            `json:"workerUid"`
	WorkerGID  int            `json:"workerGid"`
	Budget     evalapi.Budget `json:"budget"`
}

// BrokerGrant is an expiring administrator authorization for exactly one live
// process and one connection. Its ID is consumed atomically before admission.
type BrokerGrant struct {
	SchemaVersion int                     `json:"schemaVersion"`
	ID            string                  `json:"id"`
	UID           int                     `json:"uid"`
	GID           int                     `json:"gid"`
	PID           int                     `json:"pid"`
	Starttime     uint64                  `json:"starttime"`
	ExpiresAt     time.Time               `json:"expiresAt"`
	Boundary      evalapi.BoundaryContext `json:"boundary"`
}

type brokerCall struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Nonce         uint64                   `json:"nonce"`
	Method        string                   `json:"method"`
	Session       string                   `json:"session,omitempty"`
	Owner         string                   `json:"owner,omitempty"`
	Agent         string                   `json:"agent,omitempty"`
	Runtime       string                   `json:"runtime,omitempty"`
	Parent        string                   `json:"parent,omitempty"`
	Permission    string                   `json:"permission,omitempty"`
	Budget        evalapi.Budget           `json:"budget"`
	LeaseUntil    time.Time                `json:"leaseUntil,omitempty"`
	Label         string                   `json:"label,omitempty"`
	Request       *evalapi.Request         `json:"request,omitempty"`
	Callbacks     bool                     `json:"callbacks,omitempty"`
	Depth         int                      `json:"depth,omitempty"`
	Target        *evalapi.InterruptTarget `json:"target,omitempty"`
	Grant         *evalapi.Grant           `json:"grant,omitempty"`
	ArtifactID    string                   `json:"artifactId,omitempty"`
	Name          string                   `json:"name,omitempty"`
	MIME          string                   `json:"mime,omitempty"`
	Data          []byte                   `json:"data,omitempty"`
	Binding       *evalapi.BoundaryContext `json:"binding,omitempty"`
	Deadline      time.Time                `json:"deadline"`
}

type brokerReply struct {
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
	Kind  string          `json:"kind,omitempty"`
}
type brokerArtifact struct {
	Meta evalapi.Artifact `json:"meta"`
	Data []byte           `json:"data"`
}
type brokerToolCall struct {
	RequestID string         `json:"requestId"`
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

var brokerErrors = []error{evalapi.ErrDenied, evalapi.ErrClosed, evalapi.ErrInvalid, evalapi.ErrNotFound, evalapi.ErrLimit, evalapi.ErrExpired, evalapi.ErrWorker, evalapi.ErrUnsupportedCapability}

func packBrokerReply(value any, err error) brokerReply {
	if err != nil {
		r := brokerReply{Error: err.Error()}
		for _, kind := range brokerErrors {
			if errors.Is(err, kind) {
				r.Kind = kind.Error()
				break
			}
		}
		return r
	}
	b, err := json.Marshal(value)
	if err != nil {
		return brokerReply{Error: err.Error()}
	}
	return brokerReply{Value: b}
}
func decodeBrokerReply(raw json.RawMessage, out any) error {
	var r brokerReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if r.Error != "" {
		for _, kind := range brokerErrors {
			if r.Kind == kind.Error() {
				return errors.Join(kind, errors.New(r.Error))
			}
		}
		return errors.New(r.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Value, out)
}

type brokerCaller struct {
	conn      *rpcConn
	requestID string
}

func (b brokerCaller) Tools(ctx context.Context) ([]protocol.ToolSpec, error) {
	raw, err := b.conn.call(ctx, "tools", brokerToolCall{RequestID: b.requestID})
	if err != nil {
		return nil, err
	}
	var out []protocol.ToolSpec
	err = decodeBrokerReply(raw, &out)
	return out, err
}
func (b brokerCaller) Call(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
	raw, err := b.conn.call(ctx, "tool", brokerToolCall{RequestID: b.requestID, Name: name, Arguments: args})
	if err != nil {
		return protocol.ToolResult{}, err
	}
	var out protocol.ToolResult
	err = decodeBrokerReply(raw, &out)
	return out, err
}

// Broker clients may tighten host limits, never remove or raise them.
func validateBrokerBudget(want, ceiling evalapi.Budget) error {
	pairs := [][2]int64{{int64(want.WallSeconds), int64(ceiling.WallSeconds)}, {int64(want.CPUSeconds), int64(ceiling.CPUSeconds)}, {want.MemoryBytes, ceiling.MemoryBytes}, {want.OutputBytes, ceiling.OutputBytes}, {want.ArtifactBytes, ceiling.ArtifactBytes}, {want.DiskBytes, ceiling.DiskBytes}, {int64(want.MaxWorkers), int64(ceiling.MaxWorkers)}, {int64(want.MaxCallbacks), int64(ceiling.MaxCallbacks)}, {int64(want.MaxDepth), int64(ceiling.MaxDepth)}}
	for _, pair := range pairs {
		if pair[0] < 0 || (pair[1] > 0 && pair[0] > pair[1]) {
			return errors.Join(evalapi.ErrDenied, errors.New("broker resource policy cannot be widened"))
		}
	}
	return nil
}
