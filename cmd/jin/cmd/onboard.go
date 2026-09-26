package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/takaaki-s/jind-ai/internal/agent"
	"github.com/takaaki-s/jind-ai/internal/agentdocs"
	"github.com/takaaki-s/jind-ai/internal/atomicfile"
	"github.com/takaaki-s/jind-ai/internal/config"
	"github.com/takaaki-s/jind-ai/internal/daemon"
	"github.com/takaaki-s/jind-ai/internal/session"
	"github.com/takaaki-s/jind-ai/internal/worktreehook"
)

const (
	onboardingSchemaVersion = 1
	onboardingJournalName   = "onboarding.json"
	onboardingTmpPattern    = ".onboarding-*.tmp"
)

type onboardingCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Planned string `json:"planned,omitempty"`
}

type onboardingAgentPlan struct {
	Kind         string                    `json:"kind"`
	Executable   string                    `json:"executable,omitempty"`
	Capabilities session.AgentCapabilities `json:"capabilities"`
}

type onboardingHookPlan struct {
	Status     string `json:"status"`
	ScriptPath string `json:"script_path,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Planned    string `json:"planned,omitempty"`
	content    []byte
}

type onboardingTaskPlan struct {
	Requested      bool   `json:"requested"`
	Repository     string `json:"repository,omitempty"`
	Source         string `json:"source,omitempty"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	ExistingTaskID string `json:"existing_task_id,omitempty"`
	request        daemon.TaskNewRequest
}

type onboardingPlan struct {
	SchemaVersion int                     `json:"schema_version"`
	Ready         bool                    `json:"ready"`
	Mode          string                  `json:"mode"`
	Checks        []onboardingCheck       `json:"checks"`
	Agent         onboardingAgentPlan     `json:"agent"`
	Hook          onboardingHookPlan      `json:"hook"`
	SkillPaths    []string                `json:"skill_paths,omitempty"`
	Task          *onboardingTaskPlan     `json:"task,omitempty"`
	Blockers      []string                `json:"blockers,omitempty"`
	Warnings      []string                `json:"warnings,omitempty"`
	Applied       []string                `json:"applied,omitempty"`
	JournalPath   string                  `json:"journal_path"`
	TaskResult    *daemon.TaskNewResponse `json:"task_result,omitempty"`
}

type onboardingOptions struct {
	Confirm   bool
	Skill     bool
	SkillDir  string
	TrustHook bool
	AgentKind string
	Task      *daemon.TaskNewRequest
}

type onboardingJournal struct {
	SchemaVersion      int       `json:"schema_version"`
	ConfigCreated      bool      `json:"config_created,omitempty"`
	SkillPaths         []string  `json:"skill_paths,omitempty"`
	HookRepository     string    `json:"hook_repository,omitempty"`
	HookSHA256         string    `json:"hook_sha256,omitempty"`
	DaemonStarted      bool      `json:"daemon_started,omitempty"`
	TaskFingerprint    string    `json:"task_fingerprint,omitempty"`
	TaskIdempotencyKey string    `json:"task_idempotency_key,omitempty"`
	TaskID             string    `json:"task_id,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

var (
	onboardingLookPath    = exec.LookPath
	onboardingStartDaemon = startDaemonInBackground
	onboardingWaitDaemon  = waitForOnboardingDaemon
	onboardingCreateTask  = func(req daemon.TaskNewRequest) (*daemon.TaskNewResponse, error) {
		return daemon.NewClient(getSocketPath()).NewTask(req)
	}
)

var onboardCmd = &cobra.Command{
	Use:   "onboard",
	Short: "Preflight jin and optionally start the first isolated task",
	Long: `Inspect git, tmux, the selected agent, daemon identity, worktree placement,
hooks, and optional skill installation as one bounded plan.

The default is a dry run: it prints every blocker and planned write without
changing files or starting processes. Re-run the same command with --confirm
to apply the plan. Existing configuration and skill files are never replaced.

Add exactly one of --prompt, --prompt-file, or --issue to continue through the
first isolated Task. The local repository defaults to the current git root.
The durable onboarding journal stores only a prompt digest and idempotency key,
never the prompt body or credentials, so an interrupted run can be retried.`,
	Args: cobra.NoArgs,
	RunE: runOnboard,
}

func init() {
	rootCmd.AddCommand(onboardCmd)
	onboardCmd.Flags().Bool("confirm", false, "Apply the displayed plan")
	onboardCmd.Flags().Bool("skill", false, "Install the jin skill for detected agents (opt-in)")
	onboardCmd.Flags().String("skill-dir", "", "Install the skill into this directory (implies --skill)")
	onboardCmd.Flags().Bool("trust-hook", false, "Trust the displayed worktree hook content and SHA256")
	addTaskNewFlags(onboardCmd)
}

func runOnboard(cmd *cobra.Command, _ []string) error {
	opts, err := onboardingOptionsFromCommand(cmd)
	if err != nil {
		return err
	}
	plan, journal, err := buildOnboardingPlan(opts)
	if err != nil {
		return err
	}
	if opts.Confirm {
		plan.Mode = "apply"
	}

	if !plan.Ready {
		if jsonOutput {
			return fmt.Errorf("onboarding preflight blocked: %s", strings.Join(plan.Blockers, "; "))
		}
		renderOnboardingPlan(cmd.OutOrStdout(), plan)
		return fmt.Errorf("onboarding blocked by %d preflight check(s)", len(plan.Blockers))
	}
	if !opts.Confirm {
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), plan)
		}
		renderOnboardingPlan(cmd.OutOrStdout(), plan)
		fmt.Fprintln(cmd.OutOrStdout(), "\nNo changes applied. Re-run with --confirm to apply this plan.")
		return nil
	}

	if err := applyOnboardingPlan(&plan, opts, &journal); err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(cmd.OutOrStdout(), plan)
	}
	renderOnboardingPlan(cmd.OutOrStdout(), plan)
	fmt.Fprintln(cmd.OutOrStdout(), "\nOnboarding complete.")
	return nil
}

func onboardingOptionsFromCommand(cmd *cobra.Command) (onboardingOptions, error) {
	confirm, _ := cmd.Flags().GetBool("confirm")
	skill, _ := cmd.Flags().GetBool("skill")
	skillDir, _ := cmd.Flags().GetString("skill-dir")
	trustHook, _ := cmd.Flags().GetBool("trust-hook")
	agentKind, _ := cmd.Flags().GetString("agent")

	hasTask := cmd.Flags().Changed("prompt") || cmd.Flags().Changed("prompt-file") || cmd.Flags().Changed("issue")
	var req *daemon.TaskNewRequest
	if hasTask {
		cwd, err := os.Getwd()
		if err != nil {
			return onboardingOptions{}, fmt.Errorf("get current directory: %w", err)
		}
		parsed, err := taskNewRequestWithDefaultRepo(cmd, cwd)
		if err != nil {
			return onboardingOptions{}, err
		}
		if !cmd.Flags().Changed("idempotency-key") {
			// taskNewRequest normally mints a random key immediately. A plan must
			// be reproducible without writing a receipt, so onboarding derives
			// its key from the request fingerprint later instead.
			parsed.IdempotencyKey = ""
		}
		if parsed.Target != "" || parsed.Repository != "" {
			return onboardingOptions{}, fmt.Errorf("onboarding supports the first local task only; use `jin task new` for remote execution")
		}
		req = &parsed
	} else {
		for _, name := range []string{"repo", "target", "repository", "title", "workdir", "base", "model", "fleet", "no-hook", "idempotency-key"} {
			if cmd.Flags().Changed(name) {
				return onboardingOptions{}, fmt.Errorf("--%s requires --prompt, --prompt-file, or --issue", name)
			}
		}
		if trustHook {
			return onboardingOptions{}, fmt.Errorf("--trust-hook requires --prompt, --prompt-file, or --issue so the repository is explicit")
		}
	}

	return onboardingOptions{
		Confirm: confirm, Skill: skill || skillDir != "", SkillDir: skillDir,
		TrustHook: trustHook, AgentKind: agentKind, Task: req,
	}, nil
}

func buildOnboardingPlan(opts onboardingOptions) (onboardingPlan, onboardingJournal, error) {
	configDir := getConfigDir()
	stateDir := getStateDir()
	configPath := filepath.Join(configDir, "config.yaml")
	journalPath := filepath.Join(stateDir, onboardingJournalName)
	plan := onboardingPlan{
		SchemaVersion: onboardingSchemaVersion,
		Ready:         true,
		Mode:          "plan",
		JournalPath:   journalPath,
	}

	journal, err := loadOnboardingJournal(journalPath)
	if err != nil {
		plan.block("journal", err.Error())
	} else if journal.SchemaVersion == 0 {
		plan.check("journal", "missing", journalPath, "create a mode-0600 step journal on --confirm")
	} else {
		plan.check("journal", "ready", journalPath, "update after each completed step")
	}

	configMgr, configExists, err := loadOnboardingConfig(configDir, configPath)
	if err != nil {
		plan.block("config", err.Error())
	} else if configExists {
		plan.check("config", "ready", configPath, "")
	} else {
		plan.check("config", "missing", configPath, "create default config (without overwriting)")
	}

	for _, executable := range []string{"git", "tmux"} {
		path, lookupErr := onboardingLookPath(executable)
		if lookupErr != nil {
			plan.block(executable, fmt.Sprintf("%s executable not found on PATH", executable))
			continue
		}
		plan.check(executable, "ready", path, "")
	}

	agentKind := "claude"
	if configMgr != nil {
		agentKind = configMgr.GetDefaultAgent()
	}
	if opts.AgentKind != "" {
		agentKind = opts.AgentKind
	} else if opts.Task != nil && opts.Task.AgentKind != "" {
		agentKind = opts.Task.AgentKind
	}
	plan.Agent.Kind = agentKind
	agentAdapter, lookupErr := agent.Lookup(agentKind)
	if lookupErr != nil {
		plan.block("agent", lookupErr.Error())
	} else {
		plan.Agent.Capabilities = session.CapabilitiesOf(agentAdapter)
	}
	if executable, lookupErr := onboardingLookPath(agentKind); lookupErr != nil {
		plan.block("agent", fmt.Sprintf("selected agent executable %q not found on PATH", agentKind))
	} else {
		plan.Agent.Executable = executable
		plan.check("agent", "ready", fmt.Sprintf("%s (%s)", agentKind, executable), "")
	}
	if opts.Task != nil {
		opts.Task.AgentKind = agentKind
		if plan.Agent.Capabilities.State(session.CapabilityLiveness) != session.CapabilitySupported {
			plan.block("agent-capability", fmt.Sprintf("selected agent liveness capability is %s", plan.Agent.Capabilities.State(session.CapabilityLiveness)))
		}
		if plan.Agent.Capabilities.State(session.CapabilitySend) != session.CapabilitySupported {
			plan.block("agent-capability", fmt.Sprintf("selected agent send capability is %s", plan.Agent.Capabilities.State(session.CapabilitySend)))
		}
	}

	worktreeCfg := config.DefaultWorktreeConfig()
	if configMgr != nil {
		worktreeCfg = configMgr.GetWorktreeConfig()
	}
	placement := worktreeCfg.BaseDir
	if placement == "" {
		placement = filepath.Join(stateDir, "worktrees", "{name}")
	}
	if !filepath.IsAbs(os.ExpandEnv(strings.ReplaceAll(strings.ReplaceAll(placement, "{name}", "example"), "{repo}", "repo"))) {
		plan.block("worktree", fmt.Sprintf("worktree.base_dir must resolve to an absolute path: %s", placement))
	} else {
		plan.check("worktree", "ready", placement, "isolated worktrees are created here")
	}

	if opts.Skill {
		skillPlan, skillErr := resolveSkillPlan(opts.SkillDir, false)
		if skillErr != nil {
			plan.block("skill", skillErr.Error())
		} else {
			plan.SkillPaths = append(plan.SkillPaths, skillPlan.write...)
			if len(skillPlan.write) == 0 {
				plan.check("skill", "ready", "all selected skill files already exist", "")
			} else {
				plan.check("skill", "missing", strings.Join(skillPlan.write, ", "), "create without replacing existing files")
			}
			if len(skillPlan.existing) > 0 {
				plan.Warnings = append(plan.Warnings, "existing skill files will be left unchanged: "+strings.Join(skillPlan.existing, ", "))
			}
		}
	} else {
		plan.check("skill", "skipped", "opt-in only", "pass --skill to install")
	}

	daemonState, daemonErr := probeOnboardingDaemon()
	switch {
	case daemonErr != nil:
		plan.block("daemon", daemonErr.Error())
	case daemonState == "running":
		plan.check("daemon", "ready", getSocketPath(), "")
	default:
		plan.check("daemon", "stopped", getSocketPath(), "start daemon")
	}

	if opts.Task != nil {
		if _, err := onboardingLookPath("gh"); opts.Task.Issue != "" && err != nil {
			plan.block("github", "gh executable not found on PATH for --issue")
		}
		repo, rootErr := resolveLocalRepoRoot(opts.Task.Repo)
		if rootErr != nil {
			plan.block("repository", rootErr.Error())
		} else {
			opts.Task.Repo = repo
			plan.check("repository", "ready", repo, "create an isolated worktree")
			addOnboardingAgentSetupCheck(&plan, agentKind, stateDir, placement)
			plan.Hook = inspectOnboardingHook(repo, stateDir, worktreeCfg, opts)
			if plan.Hook.Status == "error" {
				plan.block("hook", plan.Hook.Planned)
			} else if opts.TrustHook && (plan.Hook.Status == "not-present" || plan.Hook.Status == "disabled") {
				plan.block("hook", "--trust-hook was requested but no active .jin/worktree-post-create.sh can be trusted")
			} else if !opts.TrustHook && (plan.Hook.Status == "untrusted" || plan.Hook.Status == "changed") {
				plan.Warnings = append(plan.Warnings, "worktree hook is not trusted and will be skipped; pass --trust-hook only after reviewing the displayed content")
			}
		}

		fingerprint, fingerprintErr := onboardingTaskFingerprint(*opts.Task)
		if fingerprintErr != nil {
			plan.block("task", fingerprintErr.Error())
		} else {
			key := journal.TaskIdempotencyKey
			if journal.TaskFingerprint != "" && journal.TaskFingerprint != fingerprint {
				plan.block("task", "the onboarding journal belongs to a different first task; use `jin task new` for additional tasks")
			}
			if key == "" {
				key = opts.Task.IdempotencyKey
			}
			if key == "" {
				key = onboardingIdempotencyKey(fingerprint)
			}
			opts.Task.IdempotencyKey = key
			plan.Task = &onboardingTaskPlan{
				Requested: true, Repository: opts.Task.Repo, Source: onboardingTaskSource(*opts.Task),
				Fingerprint: fingerprint, IdempotencyKey: key, ExistingTaskID: journal.TaskID,
				request: *opts.Task,
			}
			if journal.TaskID != "" {
				plan.check("first-task", "ready", journal.TaskID, "")
			} else {
				plan.check("first-task", "pending", opts.Task.Repo, "create task, worktree, session, and send prompt")
			}
		}
	} else {
		plan.Hook = onboardingHookPlan{Status: "not-checked", Planned: "add --prompt/--issue to inspect a repository hook"}
		plan.check("first-task", "skipped", "no prompt or issue supplied", "")
	}

	plan.Ready = len(plan.Blockers) == 0
	return plan, journal, nil
}

func addOnboardingAgentSetupCheck(plan *onboardingPlan, agentKind, stateDir, placement string) {
	switch agentKind {
	case "claude":
		home, err := os.UserHomeDir()
		if err != nil {
			plan.block("agent-setup", fmt.Sprintf("resolve home for Claude workspace trust: %v", err))
			return
		}
		plan.check(
			"agent-setup", "pending",
			filepath.Join(stateDir, "hooks-settings.json"),
			fmt.Sprintf("generate per-invocation hooks and merge trust for a new %s worktree into %s without replacing unrelated keys", placement, filepath.Join(home, ".claude.json")),
		)
	case "opencode":
		plan.check(
			"agent-setup", "pending",
			filepath.Join(stateDir, "opencode"),
			"materialize the bundled plugin under jind-ai state; user opencode config is not overwritten",
		)
	default:
		plan.check("agent-setup", "none", "selected adapter requires no setup writes", "")
	}
}

func (p *onboardingPlan) check(name, status, detail, planned string) {
	p.Checks = append(p.Checks, onboardingCheck{Name: name, Status: status, Detail: detail, Planned: planned})
}

func (p *onboardingPlan) block(name, detail string) {
	p.Checks = append(p.Checks, onboardingCheck{Name: name, Status: "blocked", Detail: detail})
	p.Blockers = append(p.Blockers, name+": "+detail)
}

func loadOnboardingConfig(configDir, configPath string) (*config.Manager, bool, error) {
	_, err := os.Stat(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect config %s: %w", configPath, err)
	}
	mgr, err := config.NewManager(configDir)
	if err != nil {
		return nil, true, fmt.Errorf("load config %s: %w", configPath, err)
	}
	return mgr, true, nil
}

func probeOnboardingDaemon() (string, error) {
	client := daemon.NewClient(getSocketPath())
	if !client.IsRunning() {
		return "stopped", nil
	}
	if _, err := client.List(); err != nil {
		return "unhealthy", fmt.Errorf("daemon socket %s accepts connections but failed protocol/liveness check: %w", getSocketPath(), err)
	}
	return "running", nil
}

func inspectOnboardingHook(repo, stateDir string, cfg config.WorktreeConfig, opts onboardingOptions) onboardingHookPlan {
	result := onboardingHookPlan{Status: "not-present"}
	if opts.Task != nil && opts.Task.NoHook {
		result.Status = "disabled"
		result.Planned = "--no-hook disables execution"
		return result
	}
	if cfg.HookEnabled != nil && !*cfg.HookEnabled {
		result.Status = "disabled"
		result.Planned = "disabled by config"
		return result
	}
	result.ScriptPath = scriptPathFor(repo)
	content, err := os.ReadFile(result.ScriptPath)
	if errors.Is(err, os.ErrNotExist) {
		return result
	}
	if err != nil {
		result.Status = "error"
		result.Planned = err.Error()
		return result
	}
	result.content = content
	sha, err := worktreehook.ComputeSHA256(result.ScriptPath)
	if err != nil {
		result.Status = "error"
		result.Planned = err.Error()
		return result
	}
	result.SHA256 = sha
	allowlist, err := worktreehook.LoadAllowlist(stateDir)
	if err != nil {
		result.Status = "error"
		result.Planned = err.Error()
		return result
	}
	entry, ok := allowlist.Get(repo)
	switch {
	case ok && entry.SHA256 == sha:
		result.Status = "trusted"
	case ok:
		result.Status = "changed"
	case opts.TrustHook:
		result.Status = "untrusted"
	default:
		result.Status = "untrusted"
	}
	if opts.TrustHook && result.Status != "trusted" {
		result.Planned = "write exact repository and SHA256 to the hook allowlist"
	}
	return result
}

func onboardingTaskFingerprint(req daemon.TaskNewRequest) (string, error) {
	req.IdempotencyKey = ""
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encode first-task fingerprint: %w", err)
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func onboardingIdempotencyKey(fingerprint string) string {
	digest := strings.TrimPrefix(fingerprint, "sha256:")
	if len(digest) > 32 {
		digest = digest[:32]
	}
	return "onboard-" + digest
}

func onboardingTaskSource(req daemon.TaskNewRequest) string {
	if req.Issue != "" {
		return "issue:" + req.Issue
	}
	return fmt.Sprintf("prompt:sha256:%x", sha256.Sum256([]byte(req.Prompt)))
}

func loadOnboardingJournal(path string) (onboardingJournal, error) {
	var journal onboardingJournal
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return journal, nil
	}
	if err != nil {
		return journal, fmt.Errorf("read onboarding journal: %w", err)
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return journal, fmt.Errorf("decode onboarding journal: %w", err)
	}
	if journal.SchemaVersion != onboardingSchemaVersion {
		return journal, fmt.Errorf("unsupported onboarding journal schema %d", journal.SchemaVersion)
	}
	return journal, nil
}

func saveOnboardingJournal(path string, journal *onboardingJournal) error {
	journal.SchemaVersion = onboardingSchemaVersion
	journal.UpdatedAt = time.Now().UTC()
	sort.Strings(journal.SkillPaths)
	journal.SkillPaths = compactStrings(journal.SkillPaths)
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode onboarding journal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create onboarding state directory: %w", err)
	}
	if err := atomicfile.Write(path, append(data, '\n'), 0o600, onboardingTmpPattern); err != nil {
		return fmt.Errorf("write onboarding journal: %w", err)
	}
	return nil
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func applyOnboardingPlan(plan *onboardingPlan, opts onboardingOptions, journal *onboardingJournal) error {
	if plan.Task != nil && journal.TaskID == "" {
		journal.TaskFingerprint = plan.Task.Fingerprint
		journal.TaskIdempotencyKey = plan.Task.IdempotencyKey
	}
	if err := saveOnboardingJournal(plan.JournalPath, journal); err != nil {
		return err
	}
	markOnboardingCheck(plan, "journal", "ready", plan.JournalPath, "updated after each completed step")

	configPath := filepath.Join(getConfigDir(), "config.yaml")
	if !fileExists(configPath) {
		created, err := createDefaultConfig(configPath)
		if err != nil {
			return err
		}
		if created {
			journal.ConfigCreated = true
			plan.Applied = append(plan.Applied, "config")
		}
		markOnboardingCheck(plan, "config", "ready", configPath, "")
		if err := saveOnboardingJournal(plan.JournalPath, journal); err != nil {
			return err
		}
	}

	for _, path := range plan.SkillPaths {
		if home, err := os.UserHomeDir(); err == nil {
			if err := checkSkillPathUnderHome(home, path); err != nil {
				return fmt.Errorf("refuse skill path %s: %w", path, err)
			}
		}
		if err := writeSkillFile(path, false); err != nil {
			if errors.Is(err, errSkillExists) {
				continue
			}
			return err
		}
		journal.SkillPaths = append(journal.SkillPaths, path)
		plan.Applied = append(plan.Applied, "skill:"+path)
		if err := saveOnboardingJournal(plan.JournalPath, journal); err != nil {
			return err
		}
	}
	if len(plan.SkillPaths) > 0 {
		markOnboardingCheck(plan, "skill", "ready", "requested skill files are installed", "")
	}

	if opts.TrustHook && plan.Hook.ScriptPath != "" && plan.Hook.Status != "trusted" {
		currentSHA, err := worktreehook.ComputeSHA256(plan.Hook.ScriptPath)
		if err != nil {
			return err
		}
		if currentSHA != plan.Hook.SHA256 {
			return fmt.Errorf("worktree hook changed after preflight; review and run onboarding again")
		}
		allowlist, err := worktreehook.LoadAllowlist(getStateDir())
		if err != nil {
			return err
		}
		if err := allowlist.Allow(plan.Task.Repository, currentSHA); err != nil {
			return err
		}
		journal.HookRepository = plan.Task.Repository
		journal.HookSHA256 = currentSHA
		plan.Hook.Status = "trusted"
		plan.Hook.Planned = ""
		plan.Applied = append(plan.Applied, "hook-trust")
		if err := saveOnboardingJournal(plan.JournalPath, journal); err != nil {
			return err
		}
	}

	state, err := probeOnboardingDaemon()
	if err != nil {
		return err
	}
	if state != "running" {
		started, _, err := onboardingStartDaemon()
		if err != nil {
			return fmt.Errorf("start daemon: %w", err)
		}
		if started {
			journal.DaemonStarted = true
			plan.Applied = append(plan.Applied, "daemon")
		}
		if err := onboardingWaitDaemon(); err != nil {
			return err
		}
		if err := saveOnboardingJournal(plan.JournalPath, journal); err != nil {
			return err
		}
		markOnboardingCheck(plan, "daemon", "ready", getSocketPath(), "")
	}

	if plan.Task != nil && journal.TaskID == "" {
		result, err := onboardingCreateTask(plan.Task.request)
		if err != nil {
			return fmt.Errorf("first task request %s: %w", plan.Task.IdempotencyKey, err)
		}
		journal.TaskID = result.Task.ID
		plan.Task.ExistingTaskID = result.Task.ID
		plan.TaskResult = result
		plan.Applied = append(plan.Applied, "first-task")
		markOnboardingCheck(plan, "first-task", "ready", result.Task.ID, "")
		markOnboardingCheck(plan, "agent-setup", "ready", "completed while starting the first task", "")
		if err := saveOnboardingJournal(plan.JournalPath, journal); err != nil {
			return err
		}
	}
	return nil
}

func markOnboardingCheck(plan *onboardingPlan, name, status, detail, planned string) {
	for i := range plan.Checks {
		if plan.Checks[i].Name == name {
			plan.Checks[i].Status = status
			plan.Checks[i].Detail = detail
			plan.Checks[i].Planned = planned
			return
		}
	}
}

func createDefaultConfig(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Stat(path)
		if statErr == nil && info.Mode().IsRegular() {
			return false, nil
		}
		return false, fmt.Errorf("refusing to replace existing non-regular config path %s", path)
	}
	if err != nil {
		return false, fmt.Errorf("create config: %w", err)
	}
	if _, err := io.WriteString(f, configTemplate); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf("write config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("close config: %w", err)
	}
	return true, nil
}

func waitForOnboardingDaemon() error {
	deadline := time.Now().Add(5 * time.Second)
	client := daemon.NewClient(getSocketPath())
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := client.List(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready at %s: %w", getSocketPath(), lastErr)
}

func renderOnboardingPlan(w io.Writer, plan onboardingPlan) {
	fmt.Fprintf(w, "Onboarding %s (ready: %t)\n", plan.Mode, plan.Ready)
	for _, check := range plan.Checks {
		fmt.Fprintf(w, "  %-12s %-8s %s", check.Name, check.Status, check.Detail)
		if check.Planned != "" {
			fmt.Fprintf(w, " -> %s", check.Planned)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  %-12s %-8s %s\n", "capabilities", "info", renderCapabilities(plan.Agent.Capabilities))
	if len(plan.SkillPaths) > 0 && plan.Mode == "plan" {
		fmt.Fprintf(w, "\n=== %s ===\n", agentdocs.SkillFileName)
		fmt.Fprint(w, agentdocs.Skill())
		fmt.Fprintln(w, "=== end ===")
	}
	if plan.Hook.ScriptPath != "" {
		fmt.Fprintf(w, "  %-12s %-8s %s", "hook", plan.Hook.Status, plan.Hook.ScriptPath)
		if plan.Hook.SHA256 != "" {
			fmt.Fprintf(w, " (sha256:%s)", plan.Hook.SHA256)
		}
		fmt.Fprintln(w)
		if len(plan.Hook.content) > 0 && plan.Hook.Planned != "" {
			fmt.Fprintln(w, "\n=== .jin/worktree-post-create.sh ===")
			_, _ = w.Write(plan.Hook.content)
			if plan.Hook.content[len(plan.Hook.content)-1] != '\n' {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "=== end ===")
		}
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", warning)
	}
	for _, blocker := range plan.Blockers {
		fmt.Fprintf(w, "Blocker: %s\n", blocker)
	}
	if len(plan.Applied) > 0 {
		fmt.Fprintf(w, "Applied: %s\n", strings.Join(plan.Applied, ", "))
	}
	fmt.Fprintf(w, "Journal: %s\n", plan.JournalPath)
	if plan.Task != nil && plan.Task.ExistingTaskID != "" {
		fmt.Fprintf(w, "First task: %s (follow: jin task info %s)\n", plan.Task.ExistingTaskID, plan.Task.ExistingTaskID)
	}
}

func renderCapabilities(capabilities session.AgentCapabilities) string {
	return fmt.Sprintf("liveness=%s send=%s respond=%s resume=%s hooks=%s transcript=%s needs-answer=%s",
		capabilities.Liveness.String(), capabilities.Send.String(), capabilities.Respond.String(),
		capabilities.Resume.String(), capabilities.Hooks.String(), capabilities.Transcript.String(),
		capabilities.ReliableNeedsAnswer.String())
}
