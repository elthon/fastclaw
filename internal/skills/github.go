package skills

import (
	"fmt"
	"net/http"
	"strings"
)

// InstallFromGitHubRepo installs a skill folder from a public GitHub repo
// identified by "owner/repo". If skillName is empty, the repo itself is
// assumed to be the skill (tarball root is extracted into
// targetDir/<repo>/). Otherwise it looks up the skill folder (at any depth)
// inside the repo and extracts it to targetDir/<skillName>/.
func InstallFromGitHubRepo(repo, skillName, targetDir string) (*Result, error) {
	repo = normalizeGitHubRepo(repo)
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("repo must be owner/repo, got %q", repo)
	}
	owner, name := parts[0], parts[1]

	client := defaultHTTPClient()
	// recordErr keeps the most informative failure across ref attempts
	// instead of the last one.
	//
	// The refs are tried in order and only one of them exists, so the
	// missing one always answers 404 — and a plain "last error wins"
	// let that 404 overwrite whatever the real branch actually said.
	// A repo whose skill folder simply had a different name reported
	// "probe HTTP 404", which reads as "this repo does not exist" and
	// sends the caller off hunting for the wrong problem.
	var lastErr error
	recordErr := func(err error) {
		if lastErr == nil || isRefMissing(lastErr) {
			lastErr = err
		}
	}
	for _, ref := range []string{"main", "master"} {
		tarURL := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s", owner, name, ref)

		subpath := ""
		dest := ""
		installedName := skillName

		if skillName == "" {
			// Whole-repo skill: find where the manifest actually lives.
			found, err := resolveWholeRepoSubpath(client, tarURL, repo)
			if err != nil {
				recordErr(err)
				continue
			}
			subpath = found
			installedName = name
			dest = fmt.Sprintf("%s/%s", strings.TrimRight(targetDir, "/"), installedName)
		} else {
			found, err := findSkillDirInTarball(client, tarURL, skillName)
			if err != nil {
				recordErr(err)
				continue
			}
			if found == "" {
				recordErr(fmt.Errorf("skill %q not found in %s/%s@%s", skillName, owner, name, ref))
				continue
			}
			subpath = found
			dest = fmt.Sprintf("%s/%s", strings.TrimRight(targetDir, "/"), skillName)
		}

		n, err := extractSubpath(client, tarURL, subpath, dest)
		if err != nil {
			recordErr(err)
			continue
		}
		if n == 0 {
			recordErr(fmt.Errorf("extracted no files from %s", tarURL))
			continue
		}
		return &Result{
			Source:       "github",
			Name:         installedName,
			Version:      ref,
			InstalledAt:  dest,
			FilesWritten: n,
		}, nil
	}
	if lastErr == nil {
		recordErr(fmt.Errorf("no main or master branch on %s", repo))
	}
	return nil, lastErr
}

// resolveWholeRepoSubpath decides which directory inside a repo tarball is
// the skill, for installs that name a repo but no skill folder.
//
// The manifest is often NOT at the repo root — plenty of single-skill
// projects keep skill/SKILL.md beside a README and examples/. Extracting
// the root verbatim produced a folder with no SKILL.md, which every loader
// ignores: the install reported success and the skill never appeared.
//
// Returns the in-tarball subpath ("" = repo root). Errors when the repo
// holds no skill at all, or holds several and the caller has to pick.
func resolveWholeRepoSubpath(client *http.Client, tarURL, repo string) (string, error) {
	roots, err := findSkillRootsInTarball(client, tarURL)
	if err != nil {
		return "", err
	}
	switch len(roots) {
	case 0:
		return "", fmt.Errorf("no SKILL.md found anywhere in %s — that repo doesn't look like an agent skill", repo)
	case 1:
		return roots[0], nil
	default:
		// A root manifest wins over nested ones: the repo is itself a
		// skill that happens to vendor others.
		if indexOf(roots, "") >= 0 {
			return "", nil
		}
		// Genuinely ambiguous — name the candidates instead of guessing.
		return "", fmt.Errorf("%s contains %d skills (%s); pass the skill folder name to pick one",
			repo, len(roots), strings.Join(baseNames(roots), ", "))
	}
}

// isRefMissing reports whether err is just "that branch isn't there".
// Only these get overwritten by a later attempt's error — anything else
// describes the repo's actual contents and is worth more than a 404 from
// the branch name that happened to be tried second.
func isRefMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "probe HTTP 404")
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

// baseNames reduces in-tarball skill dirs to the folder names a caller
// would pass back as skillName.
func baseNames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			p = p[i+1:]
		}
		out = append(out, p)
	}
	return out
}

// normalizeGitHubRepo strips common wrapper prefixes/suffixes so callers can
// pass things like "https://github.com/owner/repo.git" directly.
func normalizeGitHubRepo(repo string) string {
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimPrefix(repo, "http://github.com/")
	repo = strings.TrimPrefix(repo, "github.com/")
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.Trim(repo, "/")
	return repo
}
