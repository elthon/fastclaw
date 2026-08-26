package agent

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// newOperatorBuilder builds an Agent-mode ContextBuilder with no identity
// files at all — the exact state that used to make the model refuse
// operator work: an empty USER.md read as "unknown chatter", so it
// declined platform-management requests from the owner themselves.
func newOperatorBuilder(t *testing.T) *ContextBuilder {
	t.Helper()
	// Self-hosted branch of modAgentIntro; the hosted branch has no
	// operator concept at all.
	t.Setenv("FASTCLAW_DEPLOY", "")
	store := newFakeMemoryStore()
	mem := NewMemoryWithStoreForUser("", store, ownerUID, testAgentID)
	cb := NewContextBuilder("", mem, "")
	cb.store = store
	cb.agentID = testAgentID
	cb.userID = ownerUID
	cb.SetPromptMode(config.PromptModeAgent)
	return cb
}

func TestAgentPrompt_TrustedTurnGrantsOperatorWork(t *testing.T) {
	cb := newOperatorBuilder(t)

	prompt := cb.BuildSystemPromptAs(ownerUID, cb.memory.WithUserID(ownerUID), true)

	// The verdict must be stated, not left to inference from USER.md.
	mustContain(t, prompt, "current chatter IS this agent's operator")
	mustContain(t, prompt, "host shell")
	// ...and the model must be told not to punt it back to the chatter.
	mustContain(t, prompt, "do not ask them to run the command in their own terminal")

	// Authorization is separated from identity: an empty USER.md still
	// means "you don't know their name", and must not become a refusal.
	mustContain(t, prompt, "Never let an empty USER.md turn into a refusal of operator work")

	// The provisioning recipe — `agents config` alone was never enough to
	// get the model to `agents init`.
	mustContain(t, prompt, "fastclaw agents init")
	mustContain(t, prompt, "fastclaw skill install --agent")
	mustContain(t, prompt, "Without --agent the skill installs globally")

	// The default agent equipping a DIFFERENT agent is the whole point;
	// without this the model can read "this agent's private skills dir"
	// as "only my own" and install into itself instead.
	mustContain(t, prompt, "Installing into\n     an agent OTHER than yourself is normal")
	mustContain(t, prompt, "Never install a skill into yourself as a substitute")

	// The guest wording must not also be present — two contradictory
	// branches in one prompt is what produced the original refusal.
	mustNotContain(t, prompt, "is NOT this agent's operator")
}

func TestAgentPrompt_UntrustedTurnKeepsOperatorOnly(t *testing.T) {
	cb := newOperatorBuilder(t)

	prompt := cb.BuildSystemPromptAs(chatterUID, cb.memory.WithUserID(chatterUID), false)

	mustContain(t, prompt, "current chatter is NOT this agent's operator")
	mustContain(t, prompt, "operator-only")

	// A guest must not be handed the provisioning recipe, nor the
	// "run the CLI yourself" instruction.
	mustNotContain(t, prompt, "fastclaw agents init")
	mustNotContain(t, prompt, "current chatter IS this agent's operator")
}

// Being the operator grants authority, not a host shell. On an
// enforced-sandbox build exec lands in the container, so the agent must
// hand the commands over — claiming host access here would send it into
// a loop of exec calls that never touch the real store.
func TestAgentPrompt_EnforcedSandboxOperatorHandsOverCommands(t *testing.T) {
	cb := newOperatorBuilder(t)
	cb.sandboxEnabled = true

	prompt := cb.BuildSystemPromptAs(ownerUID, cb.memory.WithUserID(ownerUID), true)

	// Still recognized as the operator — the request is legitimate.
	mustContain(t, prompt, "current chatter IS this agent's operator")
	mustContain(t, prompt, `never answer one with "that's operator-only"`)
	// ...but told plainly it cannot run the CLI itself.
	mustContain(t, prompt, "you cannot run the")
	mustContain(t, prompt, "paste into their own terminal")
	// The host-access claim from the non-sandboxed branch must be absent.
	mustNotContain(t, prompt, "has already granted this turn host shell")

	// The recipe is still useful — it's the command list they'll paste.
	mustContain(t, prompt, "fastclaw agents init")
}

// Under `go test` the executable isn't the CLI, so the recipe must fall
// back to the bare name rather than emitting the test binary's path. This
// fallback is also what every prompt assertion above depends on.
func TestFastclawBinaryFallsBackOutsideTheCLI(t *testing.T) {
	if got := fastclawBinary(); got != "fastclaw" {
		t.Fatalf("fastclawBinary() = %q, want the bare name when the executable isn't the CLI", got)
	}
}

// The CLI enumeration is what the model reaches for when asked to do
// platform work; `agents init` missing from it was the reason "create an
// agent" never mapped onto a command.
func TestAgentPrompt_CLIListNamesAgentsInit(t *testing.T) {
	cb := newOperatorBuilder(t)

	for _, trusted := range []bool{true, false} {
		prompt := cb.BuildSystemPromptAs(ownerUID, cb.memory.WithUserID(ownerUID), trusted)
		mustContain(t, prompt, "`fastclaw agents` (init / ls / config / rm)")
		mustContain(t, prompt, "`fastclaw skill` (list / search / install)")
	}
}
