// Package config loads the taskpilot YAML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration file.
type Config struct {
	// Home holds task state, worktrees and the materialized plugin.
	Home string `yaml:"home"`
	// OperatorLanguage is the language the agent uses for summaries shown in the TUI.
	OperatorLanguage string `yaml:"operator_language"`

	Claude Claude   `yaml:"claude"`
	Daemon Daemon   `yaml:"daemon"`
	Source Source   `yaml:"source"`
	Gates  []string `yaml:"gates"`
	Rules  []Rule   `yaml:"rules"`
	// PRFlow is the default pull-request flow of every repo: each stage opens a
	// PR from the work branch into Target once Gate is approved. "$base" means
	// the repo's base branch.
	PRFlow []PRStage `yaml:"pr_flow"`
	// Projects group the repos a task may touch. A task belongs to exactly one
	// project and can open any of that project's repos, and only those.
	Projects map[string]Project `yaml:"projects"`

	HasakiWork HasakiWork `yaml:"hasaki_work"`
	Splunk     Splunk     `yaml:"splunk"`
	ArgoCD     ArgoCD     `yaml:"argocd"`

	// Path is the file the config was loaded from (not serialized).
	Path string `yaml:"-"`
}

// Claude controls how headless Claude Code sessions are launched.
type Claude struct {
	Bin string `yaml:"bin"`
	// Model and Effort run a task until the agent picks its own execution
	// profile (MCP tool set_execution_profile, after assessing the task), and
	// whenever the operator has not pinned one. Empty: Claude Code's default.
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
	// Models are the models the agent may pick for itself (aliases such as
	// fable, opus, sonnet, haiku, or full names). Default: fable, opus, sonnet.
	Models []string `yaml:"models"`
	// MaxBudgetUSD caps the cost of one run; 0 (the default) means no cap.
	MaxBudgetUSD   float64  `yaml:"max_budget_usd"`
	PermissionMode string   `yaml:"permission_mode"`
	SettingSources string   `yaml:"setting_sources"`
	ExtraArgs      []string `yaml:"extra_args"`
}

// Efforts are the effort levels Claude Code accepts, lowest first.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ValidEffort reports whether e is an effort level Claude Code accepts.
func ValidEffort(e string) bool { return slices.Contains(Efforts, e) }

// AllowsModel reports whether the agent may pick model m.
func (c Claude) AllowsModel(m string) bool { return slices.Contains(c.Models, m) }

// HasakiWork configures the Hasaki Work integration.
type HasakiWork struct {
	APIURL   string `yaml:"api_url"`
	TokenURL string `yaml:"token_url"`
	// RefreshBefore: refresh the JWT from the session cookie when less than this remains.
	RefreshBefore time.Duration `yaml:"refresh_before"`
	// Chrome used by `taskpilot auth login`: a dedicated profile, never the everyday one.
	ChromeExe     string `yaml:"chrome_exe"`
	ChromeProfile string `yaml:"chrome_profile"`
	DebugPort     int    `yaml:"debug_port"`
}

// Daemon controls the scheduling loop.
type Daemon struct {
	Concurrency  int           `yaml:"concurrency"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Source selects where tickets come from.
type Source struct {
	Type string `yaml:"type"`
	Dir  string `yaml:"dir"`
}

// Rule is a gate policy rule evaluated by the PreToolUse hook.
// Every matching rule is evaluated; any denial blocks the tool call.
type Rule struct {
	// Tool is a regex matched against the full tool name (anchored).
	Tool string `yaml:"tool"`
	// Command is an optional regex matched against tool_input.command (Bash).
	Command string `yaml:"command"`
	// Path is an optional regex matched against tool_input.file_path / path /
	// notebook_path (Read, Edit, Write, Grep, Glob, ...).
	Path string `yaml:"path"`
	// Requires names a gate that must be approved before the call is allowed.
	Requires string `yaml:"requires"`
	// Deny, when set, always blocks the call with this reason.
	Deny string `yaml:"deny"`
}

// MCPServer is an extra MCP server exposed to the agent (graphify, argocd, ...).
type MCPServer struct {
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args" json:"args,omitempty"`
	Env     map[string]string `yaml:"env" json:"env,omitempty"`
}

// ArgoCD configures the QC Argo CD instance (app status, pod logs, sync).
type ArgoCD struct {
	URL string `yaml:"url"`
	// Env names the environment this instance deploys (shown in the TUI).
	Env string `yaml:"env"`
	// LoginPath is opened by `taskpilot auth argocd`.
	LoginPath string `yaml:"login_path"`
	// MaxLogLines caps log lines per request.
	MaxLogLines int `yaml:"max_log_lines"`
}

// ProjectArgo maps a project to its Argo CD applications.
type ProjectArgo struct {
	// Apps lists application names explicitly.
	Apps []string `yaml:"apps"`
	// Projects are Argo CD AppProject names whose apps all belong here.
	Projects []string `yaml:"projects"`
	// MatchRepos also takes apps whose name contains one of the project's
	// repo names (default true).
	MatchRepos *bool `yaml:"match_repos"`
}

// Splunk configures read-only log search for agents (MCP tool splunk_search).
type Splunk struct {
	URL string `yaml:"url"`
	// APIURL is where token-authenticated REST calls go (default: URL). Some
	// deployments only expose Splunk Web on 443 and the REST API on :8089.
	APIURL string `yaml:"api_url"`
	Locale string `yaml:"locale"`
	// MaxLookback caps how far back a search may go (relative earliest times).
	MaxLookback time.Duration `yaml:"max_lookback"`
	// MaxEvents caps events returned to the agent per search.
	MaxEvents int `yaml:"max_events"`
	// DefaultEarliest is used when the agent gives no time range.
	DefaultEarliest string `yaml:"default_earliest"`
	// Timeout bounds one search.
	Timeout time.Duration `yaml:"timeout"`
	// Login page opened by `taskpilot auth splunk` in the dedicated Chrome profile.
	LoginPath string `yaml:"login_path"`
}

// PRStage is one step of a repo's pull-request flow.
type PRStage struct {
	Target string `yaml:"target" json:"target"`
	Gate   string `yaml:"gate" json:"gate"`
}

// DefaultPRFlow: feature → release/qc for testing (after the pr gate), then
// feature → base (after the qc gate). Promotion base → release/prod is a human release step.
func DefaultPRFlow() []PRStage {
	return []PRStage{{Target: "release/qc", Gate: "pr"}, {Target: "$base", Gate: "qc"}}
}

// Project is a group of repos that tasks are scoped to (BOM, PLM, payment, ...).
type Project struct {
	Name string `yaml:"name"`
	// HasakiProjectID links Hasaki Work tasks of that project to this one.
	HasakiProjectID int64 `yaml:"hasaki_project_id"`
	// ContextFiles are workspace-level knowledge files (e.g. a shared CLAUDE.md)
	// the agent reads before planning. They stay outside the task workspace.
	ContextFiles []string `yaml:"context_files"`
	// AllowedTools apply to every task of the project, on top of each repo's.
	AllowedTools []string             `yaml:"allowed_tools"`
	MCPServers   map[string]MCPServer `yaml:"mcp_servers"`
	Repos        map[string]Repo      `yaml:"repos"`
	// SplunkFilter is prepended to every Splunk search of this project's tasks,
	// e.g. 'index=k8s kubernetes.namespace_name="payment-*"'. Empty: no scoping.
	SplunkFilter string `yaml:"splunk_filter"`
	// NoLogs marks a project without logs in Splunk; splunk_search is refused.
	NoLogs bool `yaml:"no_logs"`
	// LogsProduction powers the "Logs Production" section of the TUI.
	LogsProduction *ProdLogs `yaml:"logs_production"`
	// ArgoCD links the project to its QC applications.
	ArgoCD ProjectArgo `yaml:"argocd"`
	// QCDB gives agents read-only access to the project's QC database (MCP
	// tool db_query). Nil: no database access.
	QCDB *QCDB `yaml:"qc_db"`
}

// QCDB locates the credentials of a project's QC database (MySQL). They are
// read when a query runs, from a file the project's repos keep, or — with no
// file — from the secret store (`taskpilot auth set-db PROJECT`).
type QCDB struct {
	// File holds the credentials: a KEY=VALUE env file (with Env) or a YAML
	// config (with YAML).
	File string `yaml:"file"`
	// Env is the key whose value is a MySQL DSN (user:pass@tcp(host:3306)/db).
	Env string `yaml:"env"`
	// YAML gives the dotted key paths of the connection fields.
	YAML *QCDBYAML `yaml:"yaml"`
	// Database overrides the schema of the DSN.
	Database string `yaml:"database"`
	// MaxRows caps the rows returned per query (default 200, at most 1000).
	MaxRows int `yaml:"max_rows"`
	// Timeout bounds a query (default 30s).
	Timeout time.Duration `yaml:"timeout"`
	// Notes are shown to the agent with the tool (e.g. main tables).
	Notes string `yaml:"notes"`
}

// QCDBYAML gives the dotted key paths of a YAML config's connection fields,
// e.g. host: MySql.MySqlHost.
type QCDBYAML struct {
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
}

// ProdLogs describes where a project's production logs are and the saved
// searches the operator runs most.
type ProdLogs struct {
	// Filter selects the project's production logs, e.g. kubernetes.namespace_name=payment-prod.
	Filter string `yaml:"filter"`
	// Services are the container names offered for {service}.
	Services []string `yaml:"services"`
	// Knowledge is the prod-logs skill file shown with "i" (e.g. payment.md).
	Knowledge string     `yaml:"knowledge"`
	Queries   []LogQuery `yaml:"queries"`
}

// LogQuery is a saved search. SPL may use {filter}, {service} and any other
// {name} placeholder, which the TUI asks for before running.
type LogQuery struct {
	Name     string `yaml:"name"`
	SPL      string `yaml:"spl"`
	Earliest string `yaml:"earliest"`
	Note     string `yaml:"note"`
}

// Repo describes one repository of a project.
type Repo struct {
	Path string `yaml:"path"`
	Base string `yaml:"base"`
	// Remote, when set (default "origin"), is fetched before a worktree is created
	// and the worktree starts from <remote>/<base>, so a stale or missing local
	// base branch does not matter. Set to "-" to branch from the local base.
	Remote string `yaml:"remote"`
	Verify string `yaml:"verify"`
	// PRFlow overrides the config-wide pr_flow for this repo.
	PRFlow []PRStage `yaml:"pr_flow"`
	// Description helps the agent pick the right repos for a task.
	Description  string     `yaml:"description"`
	AllowedTools []string   `yaml:"allowed_tools"`
	Prepare      [][]string `yaml:"prepare"`
}

// harnessPath matches the harness's private files: the database, the config,
// the task state, the plugin, the inbox and the dedicated Chrome profiles.
const harnessPath = `taskpilot\.db|\.config/taskpilot|taskpilot-chrome|chrome-profile` +
	`|\.taskpilot/(tasks|plugin|inbox)\b|\.taskpilot/?($|[\s"'*])` +
	`|\.\./\.\./(tasks|plugin|inbox|taskpilot)\b`

const harnessDeny = "taskpilot policy: the harness's own files (database, config, task state, browser profile) are off-limits"

// DefaultRules are used when the config defines no rules.
func DefaultRules() []Rule {
	return []Rule{
		{Tool: `Edit|Write|MultiEdit|NotebookEdit`, Requires: "plan"},
		{Tool: `Bash`, Command: `(^|[;&|(]\s*)git\s+push\b`, Requires: "pr"},
		{Tool: `Bash`, Command: `\bgh\s+pr\s+(create|merge)\b`, Requires: "pr"},
		{Tool: `mcp__taskpilot__create_pull_request`, Requires: "pr"},
		{Tool: `Bash`, Command: `\bargocd\s+app\s+(sync|rollback|delete|set)\b`, Requires: "deploy"},
		{Tool: `mcp__.*argo.*__.*(sync|rollback|delete|create|update).*`, Requires: "deploy"},
		{Tool: `Bash`, Command: `\bvault\s+(read|kv\s+get|kv\s+put|write|token\s+create)\b`, Deny: "taskpilot policy: the agent must never read or write Vault secret values"},
		{Tool: `Bash`, Command: `\bgit\s+push\b.*(--force|-f\b)`, Deny: "taskpilot policy: force-push is not allowed"},
		// The harness keeps credentials in plain text in its database: keep the agent out of
		// the harness's own files (task worktrees under ~/.taskpilot/worktrees stay allowed).
		{Tool: `.*`, Path: harnessPath, Deny: harnessDeny},
		{Tool: `Bash`, Command: harnessPath, Deny: harnessDeny},
	}
}

// Load reads and validates a config file, applying defaults.
func Load(path string) (*Config, error) {
	path = ExpandHome(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Path = abs
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Home == "" {
		c.Home = "~/.taskpilot"
	}
	c.Home = ExpandHome(c.Home)
	if c.OperatorLanguage == "" {
		c.OperatorLanguage = "Vietnamese"
	}
	if c.Claude.Bin == "" {
		c.Claude.Bin = "claude"
	}
	if c.Claude.PermissionMode == "" {
		c.Claude.PermissionMode = "dontAsk"
	}
	if len(c.Claude.Models) == 0 {
		c.Claude.Models = []string{"fable", "opus", "sonnet"}
	}
	if c.Daemon.Concurrency <= 0 {
		c.Daemon.Concurrency = 2
	}
	if c.Daemon.PollInterval <= 0 {
		c.Daemon.PollInterval = 3 * time.Second
	}
	if c.Source.Type == "" {
		c.Source.Type = "mock"
	}
	if c.Source.Dir == "" {
		c.Source.Dir = filepath.Join(c.Home, "inbox")
	}
	c.Source.Dir = ExpandHome(c.Source.Dir)
	hw := &c.HasakiWork
	if hw.APIURL == "" {
		hw.APIURL = "https://wshr.hasaki.vn/api"
	}
	if hw.TokenURL == "" {
		hw.TokenURL = "https://work.hasaki.vn/api/auth/token"
	}
	if hw.RefreshBefore <= 0 {
		hw.RefreshBefore = 6 * time.Hour
	}
	if hw.DebugPort == 0 {
		hw.DebugPort = 9222
	}
	if hw.ChromeExe == "" {
		hw.ChromeExe = "google-chrome"
		if _, err := os.Stat("/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"); err == nil {
			hw.ChromeExe = "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"
		}
	}
	// Windows Chrome (WSL) needs a Windows path; only default it for a Linux Chrome.
	if hw.ChromeProfile == "" && !strings.HasPrefix(hw.ChromeExe, "/mnt/") {
		hw.ChromeProfile = filepath.Join(c.Home, "chrome-profile")
	}
	if c.ArgoCD.URL != "" {
		c.ArgoCD.URL = strings.TrimRight(c.ArgoCD.URL, "/")
		if c.ArgoCD.Env == "" {
			c.ArgoCD.Env = "qc"
		}
		if c.ArgoCD.LoginPath == "" {
			c.ArgoCD.LoginPath = "/applications"
		}
		if c.ArgoCD.MaxLogLines <= 0 {
			c.ArgoCD.MaxLogLines = 300
		}
	}
	sp := &c.Splunk
	sp.URL = strings.TrimRight(sp.URL, "/")
	sp.APIURL = strings.TrimRight(sp.APIURL, "/")
	if sp.APIURL == "" {
		sp.APIURL = sp.URL
	}
	if sp.Locale == "" {
		sp.Locale = "en-US"
	}
	if sp.MaxLookback <= 0 {
		sp.MaxLookback = 7 * 24 * time.Hour
	}
	if sp.MaxEvents <= 0 {
		sp.MaxEvents = 100
	}
	if sp.DefaultEarliest == "" {
		sp.DefaultEarliest = "-1h"
	}
	if sp.Timeout <= 0 {
		sp.Timeout = 2 * time.Minute
	}
	if sp.LoginPath == "" {
		sp.LoginPath = "/" + sp.Locale + "/app/search/search"
	}
	if len(c.Gates) == 0 {
		c.Gates = []string{"plan", "pr", "qc", "deploy"}
	}
	if len(c.PRFlow) == 0 {
		c.PRFlow = DefaultPRFlow()
	}
	if len(c.Rules) == 0 {
		c.Rules = DefaultRules()
	}
	for pname, p := range c.Projects {
		if p.Name == "" {
			p.Name = pname
		}
		for i, f := range p.ContextFiles {
			p.ContextFiles[i] = ExpandHome(f)
		}
		if q := p.QCDB; q != nil {
			q.File = ExpandHome(q.File)
			if q.MaxRows <= 0 {
				q.MaxRows = 200
			}
			q.MaxRows = min(q.MaxRows, 1000)
			if q.Timeout <= 0 {
				q.Timeout = 30 * time.Second
			}
		}
		for name, r := range p.Repos {
			r.Path = ExpandHome(r.Path)
			if r.Base == "" {
				r.Base = "main"
			}
			if r.Remote == "" {
				r.Remote = "origin"
			}
			if len(r.PRFlow) == 0 {
				r.PRFlow = c.PRFlow
			}
			resolved := make([]PRStage, len(r.PRFlow))
			for i, st := range r.PRFlow {
				if st.Target == "$base" {
					st.Target = r.Base
				}
				resolved[i] = st
			}
			r.PRFlow = resolved
			p.Repos[name] = r
		}
		c.Projects[pname] = p
	}
}

func (c *Config) validate() error {
	gates := map[string]bool{}
	for _, g := range c.Gates {
		gates[g] = true
	}
	for i, r := range c.Rules {
		if r.Tool == "" {
			return fmt.Errorf("rules[%d]: tool is required", i)
		}
		if r.Command != "" && r.Path != "" {
			return fmt.Errorf("rules[%d]: use either command or path, not both", i)
		}
		if r.Requires == "" && r.Deny == "" {
			return fmt.Errorf("rules[%d]: one of requires/deny is required", i)
		}
		if r.Requires != "" && !gates[r.Requires] {
			return fmt.Errorf("rules[%d]: unknown gate %q (gates: %s)", i, r.Requires, strings.Join(c.Gates, ", "))
		}
	}
	hwIDs := map[int64]string{}
	for pname, p := range c.Projects {
		if !ValidName(pname) {
			return fmt.Errorf("projects.%s: name must match [A-Za-z0-9._-]+", pname)
		}
		if len(p.Repos) == 0 {
			return fmt.Errorf("projects.%s: no repos", pname)
		}
		if p.HasakiProjectID != 0 {
			if other, dup := hwIDs[p.HasakiProjectID]; dup {
				return fmt.Errorf("projects.%s: hasaki_project_id %d already used by %s", pname, p.HasakiProjectID, other)
			}
			hwIDs[p.HasakiProjectID] = pname
		}
		for srv := range p.MCPServers {
			if srv == "taskpilot" {
				return fmt.Errorf("projects.%s.mcp_servers: name %q is reserved", pname, srv)
			}
		}
		for name, r := range p.Repos {
			seen := map[string]bool{}
			for i, st := range r.PRFlow {
				if st.Target == "" || !gates[st.Gate] {
					return fmt.Errorf("projects.%s.repos.%s.pr_flow[%d]: needs a target and a configured gate (gates: %s)", pname, name, i, strings.Join(c.Gates, ", "))
				}
				if seen[st.Target] {
					return fmt.Errorf("projects.%s.repos.%s.pr_flow: target %s listed twice", pname, name, st.Target)
				}
				seen[st.Target] = true
			}
			if !ValidName(name) {
				return fmt.Errorf("projects.%s.repos.%s: name must match [A-Za-z0-9._-]+", pname, name)
			}
			if r.Path == "" {
				return fmt.Errorf("projects.%s.repos.%s: path is required", pname, name)
			}
		}
	}
	return nil
}

var validName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidName reports whether s is usable as a project or repo name (and a directory name).
func ValidName(s string) bool { return validName.MatchString(s) && s != "." && s != ".." }

// ProjectByHasakiID returns the project linked to a Hasaki Work project id.
func (c *Config) ProjectByHasakiID(id int64) (string, *Project) {
	for name, p := range c.Projects {
		if p.HasakiProjectID == id && id != 0 {
			p := p
			return name, &p
		}
	}
	return "", nil
}

// HasGate reports whether g is a configured gate.
func (c *Config) HasGate(g string) bool {
	for _, x := range c.Gates {
		if x == g {
			return true
		}
	}
	return false
}

// AllRepoNames lists every repo of every project (for Argo CD app matching).
func (c *Config) AllRepoNames() []string {
	var out []string
	for _, p := range c.Projects {
		out = append(out, p.RepoNames()...)
	}
	return out
}

// MatchesRepos reports whether apps are matched by repo name for this project.
func (pa ProjectArgo) MatchesRepos() bool { return pa.MatchRepos == nil || *pa.MatchRepos }

// LogProjects returns the projects that have a Logs Production section, sorted.
func (c *Config) LogProjects() []string {
	var out []string
	for _, n := range c.ProjectNames() {
		if p := c.Projects[n]; p.LogsProduction != nil && !p.NoLogs {
			out = append(out, n)
		}
	}
	return out
}

// ProjectNames returns configured project names, sorted.
func (c *Config) ProjectNames() []string {
	return sortedKeys(c.Projects)
}

// RepoNames returns the project's repo names, sorted.
func (p Project) RepoNames() []string { return sortedKeys(p.Repos) }

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ExpandHome expands a leading ~/ to the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// DefaultPath is where the config is looked up when --config is not given.
func DefaultPath() string {
	if p := os.Getenv("TASKPILOT_CONFIG"); p != "" {
		return p
	}
	return ExpandHome("~/.config/taskpilot/config.yaml")
}

// Stage returns the flow stage that targets branch, if any.
func (r Repo) Stage(target string) (PRStage, bool) {
	for _, st := range r.PRFlow {
		if st.Target == target {
			return st, true
		}
	}
	return PRStage{}, false
}

// AllowedToolList is the pre-approved tool list for a task of the project:
// the defaults, the project's, and every repo's (the agent may open any of
// them). Anything else is denied (--permission-prompts none); gated tools are
// allowed here and then blocked by the hook until their gate is approved.
func (p Project) AllowedToolList() []string {
	tools := []string{
		"Read", "Glob", "Grep", "Edit", "Write", "MultiEdit", "Skill", "TodoWrite",
		// Subagents (the taskpilot:* agents of the plugin, or general ones); the
		// workflow skill says when to use them. Their tool calls go through the
		// same allowlist and gate hook.
		"Agent",
		"Bash(git status*)", "Bash(git diff*)", "Bash(git log*)", "Bash(git show*)",
		"Bash(git add*)", "Bash(git commit*)", "Bash(git push*)", "Bash(git rev-parse*)",
		"Bash(git -C *)", "Bash(cd *)", "Bash(ls*)", "Bash(rg *)",
		"mcp__taskpilot",
	}
	for _, name := range sortedKeys(p.MCPServers) {
		tools = append(tools, "mcp__"+name)
	}
	seen := map[string]bool{}
	for _, t := range tools {
		seen[t] = true
	}
	add := func(ts []string) {
		for _, t := range ts {
			if !seen[t] {
				seen[t] = true
				tools = append(tools, t)
			}
		}
	}
	add(p.AllowedTools)
	for _, name := range p.RepoNames() {
		add(p.Repos[name].AllowedTools)
	}
	return tools
}
