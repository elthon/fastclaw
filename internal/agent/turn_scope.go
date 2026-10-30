package agent

import "github.com/fastclaw-ai/fastclaw/internal/agent/tools"

// Fork only mutable execution state. Session history and service dependencies
// remain shared; every turn selects its own session before using them.
func (a *Agent) forkTurn() *Agent {
	next := *a
	if a.ctxBuilder != nil {
		builder := *a.ctxBuilder
		next.ctxBuilder = &builder
	}
	if a.registry != nil {
		next.registry = a.registry.Fork()
		tools.RegisterDelegateTask(next.registry, &next)
		if next.projectRuntime != nil {
			next.registerProjectRuntimeTools()
		}
	}
	return &next
}
