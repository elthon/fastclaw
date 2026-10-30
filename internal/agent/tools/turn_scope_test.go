package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

func TestConcurrentTurnToolsKeepWorkspaceAndPermissions(t *testing.T) {
	base := NewRegistry(t.TempDir(), t.TempDir())
	base.SetWorkspaceStore(workspace.NewLocalFS(t.TempDir()), "agent")
	first, second := base.Fork(), base.Fork()
	first.SetSessionID("topic-a")
	second.SetSessionID("topic-b")
	first.SetCallerIsAdmin(true)
	second.SetCallerIsAdmin(false)
	var wg sync.WaitGroup
	for i, r := range []*Registry{first, second} {
		wg.Add(1)
		go func(i int, r *Registry) {
			defer wg.Done()
			for j := 0; j < 12; j++ {
				content := fmt.Sprintf("topic-%d-%d", i, j)
				args, _ := json.Marshal(map[string]string{"path": "report.txt", "content": content})
				// Exercise both the SDK GetFunc path and direct Execute path.
				if _, err := r.GetFunc("write_file")(context.Background(), args); err != nil {
					t.Error(err)
					return
				}
				result, err := r.Execute(context.Background(), "read_file", `{"path":"report.txt"}`)
				if err != nil || !strings.Contains(result, content) {
					t.Errorf("scope %d read %q, err %v", i, result, err)
					return
				}
			}
		}(i, r)
	}
	wg.Wait()
	if base.SessionID() != "" {
		t.Fatal("turn mutated the shared registry")
	}
	if result, _ := second.Execute(context.Background(), "write_file", `{"path":"SOUL.md","content":"overwrite"}`); !strings.Contains(result, "refused") {
		t.Fatalf("non-admin inherited another topic's permission: %s", result)
	}
	first.RecordToolFailure("exec", "{}", "failed")
	if second.PriorFailure("exec", "{}") != "" {
		t.Fatal("tool failures crossed topics")
	}
}
