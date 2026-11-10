package evalruntime

import (
	"context"
	"github.com/edynasty/LumenCortex/evalapi"
)

// Host is the existing Manager contract across an optional Linux privilege
// boundary. The broker uses Manager, not another kernel or session lifecycle.
type Host interface {
	Register(evalapi.Manifest) error
	Discover(context.Context) ([]evalapi.Manifest, error)
	Create(context.Context, evalapi.CreateOptions) (evalapi.Session, error)
	Sessions(context.Context, string) ([]evalapi.Session, error)
	Session(context.Context, string, string) (evalapi.Session, error)
	Health(context.Context, string, string) (evalapi.Health, error)
	Execute(context.Context, evalapi.Request, evalapi.ToolCaller) (evalapi.Result, error)
	InterruptExecution(context.Context, string, string, evalapi.InterruptTarget) (evalapi.InterruptOutcome, error)
	Reset(context.Context, string, string) error
	Close(context.Context, string, string) error
	Grant(context.Context, string, string, evalapi.Grant) error
	Revoke(context.Context, string, string, string) error
	Results(context.Context, string, string) ([]evalapi.Result, error)
	Artifact(context.Context, string, string, string) (evalapi.Artifact, []byte, error)
	PutArtifact(context.Context, string, string, string, string, []byte) (evalapi.Artifact, error)
	CloseOwner(context.Context, string) error
	ForgetOwner(context.Context, string) error
	RevokeAgent(context.Context, string) error
	Dispose(context.Context) error
	AssertBoundary(context.Context, string, string, evalapi.BoundaryContext) (evalapi.BoundaryReport, error)
}

// KernelInspector is an optional Host extension: a kernel can list the names it
// holds without running a cell. It is separate from Host because a remote host
// (the Linux UID broker) does not expose it yet; the Engine reports that as an
// unsupported capability instead of pretending the state is empty.
type KernelInspector interface {
	Inspect(context.Context, string, string, evalapi.InspectOptions) (evalapi.Inspection, error)
}

// KernelRestarter is an optional Host extension: replace a kernel's process and
// discard its live state while keeping its workspace files.
type KernelRestarter interface {
	Restart(context.Context, string, string) error
}

var (
	_ Host            = (*Manager)(nil)
	_ KernelInspector = (*Manager)(nil)
	_ KernelRestarter = (*Manager)(nil)
)
