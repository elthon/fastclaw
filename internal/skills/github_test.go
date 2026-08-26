package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarGz builds a codeload-shaped archive: every entry is nested under a
// synthetic "<repo>-<sha>/" top-level dir, exactly like the tarballs
// codeload.github.com serves.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     "repo-deadbeef/" + name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveTarball stands in for codeload. Only the "main" ref exists, so a
// caller that falls through to "master" gets a 404 like the real thing.
func serveTarball(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()
	body := tarGz(t, files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/main") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFindSkillRootsInTarball(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "manifest at repo root",
			files: map[string]string{"SKILL.md": "# s", "README.md": "r"},
			want:  []string{""},
		},
		{
			// The anthropic-art layout: README + examples at the root,
			// the actual skill one level down.
			name: "manifest in a subfolder",
			files: map[string]string{
				"README.md":                "r",
				"examples/earth.png":       "img",
				"skill/SKILL.md":           "# s",
				"skill/references/spec.md": "spec",
			},
			want: []string{"skill"},
		},
		{
			name: "multi-skill repo",
			files: map[string]string{
				"skills/alpha/SKILL.md": "# a",
				"skills/beta/SKILL.md":  "# b",
			},
			want: []string{"skills/alpha", "skills/beta"},
		},
		{
			name:  "not a skill repo",
			files: map[string]string{"README.md": "r", "main.go": "package main"},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveTarball(t, tc.files)
			got, err := findSkillRootsInTarball(srv.Client(), srv.URL+"/main")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got roots %q, want %q", got, tc.want)
			}
			for _, w := range tc.want {
				if indexOf(got, w) < 0 {
					t.Errorf("missing root %q in %q", w, got)
				}
			}
		})
	}
}

// resolveFrom drives the real resolver against a local tarball server.
// InstallFromGitHubRepo hardcodes the codeload host, so the whole-repo
// resolution is reached through this seam rather than the full install.
func resolveFrom(t *testing.T, srv *httptest.Server) (string, error) {
	t.Helper()
	return resolveWholeRepoSubpath(srv.Client(), srv.URL+"/main", "owner/repo")
}

// A whole-repo install must land SKILL.md at the TOP of the installed
// folder. Extracting the repo root verbatim was the bug: the skill
// installed "successfully" but had no SKILL.md where the loader looks,
// so it never appeared in any skill list.
func TestWholeRepoInstallPutsManifestAtRoot(t *testing.T) {
	srv := serveTarball(t, map[string]string{
		"README.md":                "r",
		"examples/earth.png":       "img",
		"skill/SKILL.md":           "# anthropic-art",
		"skill/references/spec.md": "spec",
	})

	subpath, err := resolveFrom(t, srv)
	if err != nil {
		t.Fatalf("resolve subpath: %v", err)
	}
	if subpath != "skill" {
		t.Fatalf("subpath = %q, want %q", subpath, "skill")
	}

	dest := filepath.Join(t.TempDir(), "anthropic-art")
	n, err := extractSubpath(srv.Client(), srv.URL+"/main", subpath, dest)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if n != 2 {
		t.Fatalf("wrote %d files, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md not at the root of the installed dir: %v", err)
	}
	// The repo's non-skill baggage must stay out.
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err == nil {
		t.Error("README.md from the repo root should not have been installed")
	}
}

func TestWholeRepoInstallRejectsAmbiguousAndEmpty(t *testing.T) {
	multi := serveTarball(t, map[string]string{
		"skills/alpha/SKILL.md": "# a",
		"skills/beta/SKILL.md":  "# b",
	})
	if _, err := resolveFrom(t, multi); err == nil {
		t.Fatal("expected an error naming the candidate skills")
	} else if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Errorf("error should name both skills, got: %v", err)
	}

	none := serveTarball(t, map[string]string{"README.md": "r"})
	if _, err := resolveFrom(t, none); err == nil {
		t.Fatal("expected an error for a repo with no SKILL.md")
	}

	// A root manifest wins over nested ones — the repo is a skill that
	// happens to vendor others.
	rootPlusNested := serveTarball(t, map[string]string{
		"SKILL.md":              "# main",
		"skills/alpha/SKILL.md": "# a",
	})
	got, err := resolveFrom(t, rootPlusNested)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("subpath = %q, want the repo root", got)
	}
}

func TestNormalizeGitHubRepo(t *testing.T) {
	for _, in := range []string{
		"owner/repo",
		"https://github.com/owner/repo",
		"https://github.com/owner/repo.git",
		"github.com/owner/repo/",
	} {
		if got := normalizeGitHubRepo(in); got != "owner/repo" {
			t.Errorf("normalizeGitHubRepo(%q) = %q, want owner/repo", in, got)
		}
	}
}
