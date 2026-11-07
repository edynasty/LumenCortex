//go:build !darwin

package evalruntime

// startForkWatcher has no kernel event source off macOS; descendant discovery
// there relies on the 150ms process-table scan only.
func startForkWatcher(p *process) func() { return func() {} }
