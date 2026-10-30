package store

import (
	"context"
	"path/filepath"
	"testing"
)

// Dev databases that ran the removed "apps" layer get the apps table and
// the app_id columns dropped; their agents and keys stay.
func TestMigrateDropsApps(t *testing.T) {
	ctx := context.Background()
	st, err := NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "apps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, q := range []string{
		`CREATE TABLE agents (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			config TEXT NOT NULL DEFAULT '{}', is_public BOOLEAN NOT NULL DEFAULT FALSE,
			app_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE INDEX idx_agents_app ON agents (app_id)`,
		`CREATE TABLE apikeys (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			key_hash TEXT NOT NULL, key_prefix TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT 'agent',
			app_id TEXT NOT NULL DEFAULT '', created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE INDEX idx_apikeys_app ON apikeys (app_id)`,
		`CREATE TABLE apps (id TEXT PRIMARY KEY, owner_user_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO apps (id, owner_user_id, name) VALUES ('app_x', 'u_a', 'snapok')`,
		`INSERT INTO agents (id, user_id, name, app_id) VALUES ('agt_1', 'u_a', 'one', 'app_x')`,
		`INSERT INTO apikeys (id, user_id, key_hash, type, app_id) VALUES ('k_1', 'u_a', 'h', 'agent', 'app_x')`,
	} {
		if _, err := st.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}
	for _, table := range []string{"agents", "apikeys"} {
		if has, _ := st.tableHasColumn(ctx, table, "app_id"); has {
			t.Fatalf("%s.app_id still present", table)
		}
	}
	if _, err := st.DB().ExecContext(ctx, `SELECT 1 FROM apps`); err == nil {
		t.Fatal("apps table still present")
	}
	if ag, err := st.GetAgent(ctx, "agt_1"); err != nil || ag.UserID != "u_a" {
		t.Fatalf("agent lost: %+v %v", ag, err)
	}
	if key, err := st.GetAPIKey(ctx, "k_1"); err != nil || key.UserID != "u_a" {
		t.Fatalf("key lost: %+v %v", key, err)
	}
}
