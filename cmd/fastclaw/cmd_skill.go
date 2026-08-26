package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/agentcli"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
)

// skillCmd handles skill management subcommands.
func skillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skills",
	}
	cmd.AddCommand(skillListCmd())
	cmd.AddCommand(skillSearchCmd())
	cmd.AddCommand(skillInstallCmd())
	cmd.AddCommand(skillUpdateCmd())
	cmd.AddCommand(skillRemoveCmd())
	cmd.AddCommand(skillInfoCmd())
	// Same treatment as the agents tree: an install failure should print
	// the reason, not a usage dump. Matters more here than usual — the
	// agent runs these through exec and feeds stderr back to the model.
	silenceTree(cmd)
	return cmd
}

func skillListCmd() *cobra.Command {
	var agentRef string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all discovered skills with source",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			var globalCfg config.SkillsCfg

			// Agent dir "." lists only the global + bundled layers. With
			// --agent we point it at that agent's home so its private
			// skills show up too — the verification step after
			// `skill install --agent`.
			agentDir := "."
			if agentRef != "" {
				dir, _, err := resolveSkillTarget(agentRef)
				if err != nil {
					return err
				}
				agentDir = filepath.Dir(dir)
			}

			loader := agent.NewSkillsLoaderWithGlobal(homeDir, agentDir, "", config.SkillsConfig{}, globalCfg)
			loaded := loader.LoadSkills()

			if len(loaded) == 0 {
				fmt.Println("No skills discovered.")
				return nil
			}

			fmt.Printf("%-25s %-20s %s\n", "NAME", "SOURCE", "DESCRIPTION")
			fmt.Println(strings.Repeat("-", 75))
			for _, s := range loaded {
				desc := s.Description
				if len(desc) > 40 {
					desc = desc[:37] + "..."
				}
				fmt.Printf("%-25s %-20s %s\n", s.Name, s.Layer, desc)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentRef, "agent", "", "also list skills private to this agent (agent name or agt_ id)")
	return cmd
}

func skillSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Search the ClawHub skill registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := skills.NewClawHubClient()
			results, err := client.Search(args[0])
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			if len(results) == 0 {
				fmt.Println("No skills found.")
				return nil
			}

			fmt.Printf("%-25s %-10s %-10s %s\n", "SLUG", "VERSION", "DOWNLOADS", "DESCRIPTION")
			fmt.Println(strings.Repeat("-", 80))
			for _, s := range results {
				desc := s.Description
				if len(desc) > 35 {
					desc = desc[:32] + "..."
				}
				fmt.Printf("%-25s %-10s %-10d %s\n", s.Slug, s.Version, s.Downloads, desc)
			}
			return nil
		},
	}
}

func skillInstallCmd() *cobra.Command {
	var version, agentRef, repo, source string
	cmd := &cobra.Command{
		Use:   "install [slug]",
		Short: "Install a skill globally or into one agent",
		Long: `Install a skill from skills.sh, ClawHub, or a GitHub repo.

Without --agent the skill lands in the shared ~/.fastclaw/skills/ and every
agent sees it. With --agent it lands in that agent's private skills dir and
stays scoped to it — this is how you provision a new agent with a specific
capability:

  fastclaw agents init illustrator
  fastclaw skill install --agent illustrator --repo HalfAI1102/anthropic-art
`,
		Example: `  fastclaw skill install pdf
  fastclaw skill install --agent agt_1a2b3c --repo owner/repo
  fastclaw skill install my-skill --agent illustrator --repo owner/monorepo`,
		// MaximumNArgs, not ExactArgs: --repo installs a whole-repo skill
		// with no slug of its own.
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := ""
			if len(args) > 0 {
				slug = args[0]
			}
			if slug == "" && repo == "" {
				return fmt.Errorf("give a skill slug, or --repo owner/repo")
			}

			targetDir, agentID, err := resolveSkillTarget(agentRef)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				return fmt.Errorf("create skills dir: %w", err)
			}

			// --version is a ClawHub-only concept, so pinning a version
			// keeps the legacy ClawHub client path. Everything else goes
			// through the shared dispatcher the HTTP API uses.
			if version != "" {
				if repo != "" {
					return fmt.Errorf("--version applies to ClawHub installs only; drop it when using --repo")
				}
				fmt.Printf("Installing %s@%s...\n", slug, version)
				if err := skills.NewClawHubClient().Install(slug, version, targetDir); err != nil {
					return fmt.Errorf("install failed: %w", err)
				}
				reportSkillInstalled(slug, "clawhub", filepath.Join(targetDir, slug), agentID)
				return nil
			}

			if repo != "" {
				fmt.Printf("Installing from github.com/%s...\n", repo)
			} else {
				fmt.Printf("Installing %s...\n", slug)
			}
			res, err := skills.Install(source, slug, repo, targetDir)
			if err != nil {
				return fmt.Errorf("install failed: %w", err)
			}
			reportSkillInstalled(res.Name, res.Source, res.InstalledAt, agentID)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "specific version to install (ClawHub only)")
	cmd.Flags().StringVar(&agentRef, "agent", "", "install into this agent's private skills dir (agent name or agt_ id) instead of globally")
	cmd.Flags().StringVar(&repo, "repo", "", "install from a GitHub 'owner/repo' instead of the public registries")
	cmd.Flags().StringVar(&source, "source", "", "force a registry: skillssh | clawhub | github (default: auto)")
	return cmd
}

// resolveSkillTarget maps an optional agent reference to the directory the
// skill should be written to. Empty ref = the shared global dir. A non-empty
// ref is resolved through the operator's store (by name or agt_ id) so the
// CLI accepts the same references as the rest of the `agents` tree, and
// returns the resolved agent id for the caller's reload/report step.
func resolveSkillTarget(agentRef string) (dir string, agentID string, err error) {
	if agentRef == "" {
		home, err := config.HomeDir()
		if err != nil {
			return "", "", err
		}
		return filepath.Join(home, "skills"), "", nil
	}
	st, err := openStoreFromEnv()
	if err != nil {
		return "", "", err
	}
	defer st.Close()
	rec, err := agentcli.Resolve(context.Background(), st, agentRef)
	if err != nil {
		return "", "", err
	}
	homePath, err := config.AgentHomeDir(rec.ID)
	if err != nil {
		return "", "", fmt.Errorf("resolve agent home: %w", err)
	}
	return filepath.Join(homePath, "skills"), rec.ID, nil
}

// reportSkillInstalled prints the result and, for per-agent installs,
// signals the running gateway so the target agent picks the skill up on
// its next turn instead of after a restart.
func reportSkillInstalled(name, source, path, agentID string) {
	if agentID == "" {
		fmt.Printf("Skill %q installed from %s to %s (all agents)\n", name, source, path)
		return
	}
	fmt.Printf("Skill %q installed from %s to %s (agent %s only)\n", name, source, path, agentID)
	notifyGatewayReload()
}

func skillUpdateCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "update [slug]",
		Short: "Update installed skills",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			targetDir := filepath.Join(homeDir, "skills")
			client := skills.NewClawHubClient()

			if all {
				installed, err := skills.ListInstalled(targetDir)
				if err != nil {
					return err
				}
				if len(installed) == 0 {
					fmt.Println("No installed skills to update.")
					return nil
				}
				for _, s := range installed {
					fmt.Printf("Updating %s...\n", s.Name)
					if err := client.Update(s.Name, targetDir); err != nil {
						fmt.Printf("  Failed: %v\n", err)
					} else {
						fmt.Printf("  Updated %s\n", s.Name)
					}
				}
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("specify a skill slug or use --all")
			}

			slug := args[0]
			fmt.Printf("Updating %s...\n", slug)
			if err := client.Update(slug, targetDir); err != nil {
				return fmt.Errorf("update failed: %w", err)
			}
			fmt.Printf("Skill %q updated.\n", slug)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "update all installed skills")
	return cmd
}

func skillRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			skillDir := filepath.Join(homeDir, "skills", name)
			if _, err := os.Stat(skillDir); os.IsNotExist(err) {
				return fmt.Errorf("skill %q not found at %s", name, skillDir)
			}

			if err := os.RemoveAll(skillDir); err != nil {
				return fmt.Errorf("remove skill: %w", err)
			}

			fmt.Printf("Skill %q removed.\n", name)
			return nil
		},
	}
}

func skillInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <slug>",
		Short: "Show skill details from the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := skills.NewClawHubClient()
			info, err := client.Info(args[0])
			if err != nil {
				return err
			}

			fmt.Printf("Name:        %s\n", info.Name)
			fmt.Printf("Slug:        %s\n", info.Slug)
			fmt.Printf("Version:     %s\n", info.Version)
			fmt.Printf("Description: %s\n", info.Description)
			fmt.Printf("Downloads:   %d\n", info.Downloads)
			return nil
		},
	}
}
