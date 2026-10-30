package tools

import "context"

type turnRegistryKey struct{}

// forContext resolves the calling turn even for tools registered at agent boot.
func (r *Registry) forContext(ctx context.Context) *Registry {
	if scoped, ok := ctx.Value(turnRegistryKey{}).(*Registry); ok {
		return scoped
	}
	return r
}

// Fork creates independent tool bindings and failure tracking for one turn.
// Background shell handles and immutable service dependencies remain shared.
func (r *Registry) Fork() *Registry {
	next := &Registry{tools: make(map[string]registeredTool, len(r.tools)),
		sandboxRoot:      r.sandboxRoot,
		executor:         r.executor,
		systemRoot:       r.systemRoot,
		userRoot:         r.userRoot,
		workspaceStore:   r.workspaceStore,
		agentID:          r.agentID,
		sessionID:        r.sessionID,
		projectID:        r.projectID,
		codingRootScope:  r.codingRootScope,
		codingSubdir:     r.codingSubdir,
		messageChannel:   r.messageChannel,
		messageAccountID: r.messageAccountID,
		messageChatID:    r.messageChatID,
		goalSessionKey:   r.goalSessionKey,
		systemFileStore:  r.systemFileStore,
		userID:           r.userID,
		chatterUserID:    r.chatterUserID,
		agentOwnerUserID: r.agentOwnerUserID,
		userSkillsRoot:   r.userSkillsRoot,
		sandboxRequired:  r.sandboxRequired,
		sandboxProvider:  r.sandboxProvider,
		callerIsAdmin:    r.callerIsAdmin,
		envProvider:      r.envProvider,
		skillDirs:        r.skillDirs,
		shellMgr:         r.shellMgr,
	}
	for name, tool := range r.tools {
		next.tools[name] = tool
	}
	return next
}
