package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceSuffix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for in, want := range map[string]string{
		"":                                       "",
		filepath.Join(home, ".fastclaw"):         "",
		filepath.Join(home, ".fastclaw-dev"):     "-dev",
		"/srv/fastclaw_staging":                  "-staging",
		"/data/other box":                        "-other-box",
		filepath.Join(home, ".fastclaw") + "/x/": "-x",
	} {
		t.Setenv("FASTCLAW_HOME", in)
		if got := instanceSuffix(); got != want {
			t.Errorf("instanceSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServiceTemplatesForwardInstanceEnv(t *testing.T) {
	env := [][2]string{{"FASTCLAW_HOME", "/h/.fastclaw-dev"}, {"FASTCLAW_PORT", "18955"}}

	var plist strings.Builder
	if err := launchdPlistTemplate.Execute(&plist, map[string]any{
		"Label": "l", "BinaryPath": "/b", "LogDir": "/l", "HomeDir": "/h", "Env": env,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist.String(), "<key>FASTCLAW_PORT</key>\n        <string>18955</string>") {
		t.Errorf("plist missing env:\n%s", plist.String())
	}

	var unit strings.Builder
	if err := systemdUnitTemplate.Execute(&unit, map[string]any{
		"BinaryPath": "/b", "HomeDir": "/h", "LogDir": "/l", "Env": env,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit.String(), "Environment=FASTCLAW_HOME=/h/.fastclaw-dev\n") {
		t.Errorf("unit missing env:\n%s", unit.String())
	}
}
