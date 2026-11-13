// Command taskpilot runs ticket-driven Claude Code sessions with human approval gates.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/claudeai"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/daemon"
	"github.com/buonnguwaaa/taskpilot/internal/gate"
	"github.com/buonnguwaaa/taskpilot/internal/graphify"
	"github.com/buonnguwaaa/taskpilot/internal/hook"
	"github.com/buonnguwaaa/taskpilot/internal/intake"
	"github.com/buonnguwaaa/taskpilot/internal/jenkins"
	"github.com/buonnguwaaa/taskpilot/internal/kafbat"
	"github.com/buonnguwaaa/taskpilot/internal/mcpserver"
	"github.com/buonnguwaaa/taskpilot/internal/plugin"
	"github.com/buonnguwaaa/taskpilot/internal/refs"
	"github.com/buonnguwaaa/taskpilot/internal/runner"
	"github.com/buonnguwaaa/taskpilot/internal/scan"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/vault"
)

//go:embed example-config.yaml
var exampleConfig []byte

const usage = `taskpilot - ticket-driven Claude Code sessions with human approval gates

Usage:
  taskpilot                                     interactive console (TUI) with the daemon built in
  taskpilot init                                write an example config to ~/.config/taskpilot/config.yaml
  taskpilot doctor                              check the config against this machine (repos, tools, auth)
  taskpilot daemon [-once]                      pull tickets and run sessions
  taskpilot tui [-no-daemon]                    same as running taskpilot with no command
  taskpilot list                                print tasks
  taskpilot submit -project P -title T [-repo R] [-body B] [-id ID]   add a ticket to the mock inbox
  taskpilot submit -prompt "…" [-yes] [-project P] [-repo R]   let Claude draft the ticket from a free-form request
  taskpilot approve TASK [-comment C]           approve the pending request
  taskpilot reject TASK -comment C              reject the pending request
  taskpilot message TASK -text M                resume a stopped task with a message
  taskpilot takeover TASK                       drive the task's Claude session by hand (daemon leaves it alone)
  taskpilot db [stats|tasks|runs TASK|sql "SELECT …"]   inspect the database (read-only)
  taskpilot export [-o DIR] [-secrets]          pack data (no tokens) for another machine; -secrets: tokens in a separate archive
  taskpilot import -i DATA.tar.gz [-secrets SECRETS.tar.gz] [-force]   restore an export on this machine

Hasaki Work:
  taskpilot auth status|login|set-token|set-cookie|refresh
  taskpilot hw me|tasks|show ID

Bitbucket:
  taskpilot auth bitbucket                      store email + API token (validated, never echoed)
  taskpilot bb pr-targets                       where recent PRs of each repo were merged, next to its pr_flow

Splunk:
  taskpilot auth splunk [-token|-cookie]        log in (Chrome profile) or paste a token / cookie
  taskpilot splunk search "<SPL>" [-earliest -1h] [-project P]   read-only search

QC databases (read-only, agents use the MCP tool db_query):
  taskpilot auth set-db PROJECT                 store a project's QC DSN (when no repo file has it)
  taskpilot qcdb list                           where each project's QC database is (no credentials shown)
  taskpilot qcdb query PROJECT "SELECT …"       run one read-only statement

Graphify graphs (graphify: graphify per repo and line, gRPC links between repos; agents use graphify):
  taskpilot graphify build [PROJECT] [-line L] [-force] build / update now (the daemon does it every graphify.refresh)
  taskpilot graphify status [PROJECT]              what each line was built from
  taskpilot graphify links PROJECT [-line L] [FILTER]   gRPC calls between the repos
  taskpilot graphify ask PROJECT LINE ACTION ARG…  query / explain / path / affected, as the agent does
  taskpilot graphify task TASK [ACTION ARG…]    build / update a task's own graph (its branch), then ask it

Production error scans (logs_production.scan: times, min_count, ignore):
  taskpilot scan run PROJECT                    scan now (since the previous scan, at most max_window)
  taskpilot scan list [PROJECT]                 fix suggestions waiting for you

Vault (QC, read-only; agents use qc_config_list / qc_config_get within vault_paths):
  taskpilot auth vault [-token]                 log in with SSO (Chrome profile) or paste a token
  taskpilot vault whoami|ls [PATH]|get PATH     look at the QC configuration yourself

Kafka (QC through Kafka UI, read-only; agents use qc_kafka_* within kafka_topics):
  taskpilot auth kafka [-token]                 log in with SSO (Chrome profile) or paste the Cookie
  taskpilot kafka whoami|topics|topic|messages|groups|group   look at QC topics yourself

claude.ai artifacts (prototypes linked from tasks; agents use read_claude_artifact):
  taskpilot auth claude                         paste the claude.ai sessionKey cookie
  taskpilot claude whoami|artifact URL [-o DIR] check the session, download an artifact yourself

Argo CD (QC):
  taskpilot auth argocd [-token]                log in (Chrome profile) or paste a token
  taskpilot auth jenkins                        store the Jenkins user + API token (read-only)
  taskpilot argo apps [-project P]              applications and the project each maps to
  taskpilot argo logs APP [-filter S]           latest pod logs

Internal (launched by Claude Code):
  taskpilot mcp  -task ID                       MCP server for one task
  taskpilot hook -task ID                       PreToolUse gate hook

Every command accepts -config PATH (default $TASKPILOT_CONFIG or ~/.config/taskpilot/config.yaml).
`

func main() {
	if len(os.Args) < 2 || strings.HasPrefix(os.Args[1], "-") && os.Args[1] != "-h" && os.Args[1] != "--help" {
		// No command (only flags): the interactive console with the scheduler embedded.
		if err := cmdTUI(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "taskpilot:", err)
			os.Exit(1)
		}
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "daemon":
		err = cmdDaemon(args)
	case "tui":
		err = cmdTUI(args)
	case "list":
		err = cmdList(args)
	case "submit":
		err = cmdSubmit(args)
	case "approve", "reject":
		err = cmdDecide(cmd, args)
	case "message":
		err = cmdMessage(args)
	case "doctor":
		err = cmdDoctor(args)
	case "auth":
		err = cmdAuth(args)
	case "bb":
		err = cmdBB(args)
	case "splunk":
		err = cmdSplunk(args)
	case "takeover":
		err = cmdTakeover(args)
	case "export":
		err = cmdExport(args)
	case "import":
		err = cmdImport(args)
	case "db":
		err = cmdDB(args)
	case "argo", "argocd":
		err = cmdArgo(args)
	case "hw":
		err = cmdHW(args)
	case "qcdb":
		err = cmdQCDB(args)
	case "vault":
		err = cmdVault(args)
	case "kafka":
		err = cmdKafka(args)
	case "claude":
		err = cmdClaude(args)
	case "graphify", "graph":
		err = cmdGraph(args)
	case "scan":
		err = cmdScan(args)
	case "mcp":
		err = cmdMCP(args)
	case "hook":
		err = cmdHook(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "taskpilot:", err)
		os.Exit(1)
	}
}

type common struct {
	fs     *flag.FlagSet
	config *string
}

func newFlags(name string) common {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return common{fs: fs, config: fs.String("config", config.DefaultPath(), "config file")}
}

// parse parses the flags of a subcommand whose positional arguments may come
// before the flags (taskpilot kafka messages TOPIC -limit 5) and returns them.
func (c common) parse(args []string) []string {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	c.fs.Parse(args)
	return append(pos, c.fs.Args()...)
}

func (c common) load() (*config.Config, *store.Store, error) {
	cfg, err := config.Load(*c.config)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.New(cfg.Home)
	if err != nil {
		return nil, nil, err
	}
	return cfg, st, nil
}

func cmdInit(args []string) error {
	c := newFlags("init")
	force := c.fs.Bool("force", false, "overwrite an existing config")
	c.fs.Parse(args)
	path := config.ExpandHome(*c.config)
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, exampleConfig, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s — edit the repos section, then run `taskpilot daemon` and `taskpilot tui`\n", path)
	return nil
}

func cmdDaemon(args []string) error {
	c := newFlags("daemon")
	once := c.fs.Bool("once", false, "run one scheduling pass, wait for the runs it started, then exit")
	c.fs.Parse(args)
	cfg, st, err := c.load()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	d, err := newDaemon(cfg, st, exe, os.Stderr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(cfg.Projects) == 0 {
		return fmt.Errorf("no projects configured in %s", cfg.Path)
	}
	unlock, err := daemon.Lock(cfg.Home)
	if err != nil {
		return fmt.Errorf("%w (close the TUI, or the other `taskpilot daemon`)", err)
	}
	defer unlock()
	if *once {
		return d.Once(ctx)
	}
	return d.Run(ctx)
}

func cmdList(args []string) error {
	c := newFlags("list")
	c.fs.Parse(args)
	_, st, err := c.load()
	if err != nil {
		return err
	}
	tasks, err := st.ListTasks()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tRUNS\tCOST\tPENDING\tTITLE")
	for _, t := range tasks {
		pending := "-"
		if r, _ := st.PendingRequest(t.ID); r != nil {
			pending = r.ID
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t$%.2f\t%s\t%s\n", t.ID, t.State, t.Runs, t.CostUSD, pending, t.Title)
	}
	return w.Flush()
}

func cmdSubmit(args []string) error {
	c := newFlags("submit")
	project := c.fs.String("project", "", "project name from the config (the task's scope)")
	repo := c.fs.String("repo", "", "optional: repo of the project to open up front")
	title := c.fs.String("title", "", "ticket title")
	body := c.fs.String("body", "", "ticket body (acceptance criteria, reproduction, ...)")
	id := c.fs.String("id", "", "ticket id (default: timestamp)")
	prompt := c.fs.String("prompt", "", `free-form request; Claude drafts project, repo, title and body ("-": read stdin)`)
	yes := c.fs.Bool("yes", false, "with -prompt: submit the draft without asking")
	c.fs.Parse(args)
	cfg, _, err := c.load()
	if err != nil {
		return err
	}
	if cfg.Source.Type != "mock" {
		return fmt.Errorf("submit only works with the mock source (configured: %s)", cfg.Source.Type)
	}
	if *prompt != "" {
		tk, ok, err := draftTicket(cfg, *prompt, *yes)
		if err != nil || !ok {
			return err
		}
		// Explicit flags win over the draft.
		for dst, v := range map[*string]string{project: tk.Project, repo: tk.Repo, title: tk.Title, body: tk.Body} {
			if *dst == "" {
				*dst = v
			}
		}
		if *project == "" {
			return fmt.Errorf("Claude could not tell the project; pass -project (configured: %v)", cfg.ProjectNames())
		}
	}
	if *title == "" {
		*title = source.TitleFrom(*body)
	}
	if *project == "" || *title == "" {
		return fmt.Errorf("-project and -title (or -body) are required")
	}
	proj, ok := cfg.Projects[*project]
	if !ok {
		return fmt.Errorf("unknown project %q (configured: %v)", *project, cfg.ProjectNames())
	}
	if _, ok := proj.Repos[*repo]; *repo != "" && !ok {
		return fmt.Errorf("repo %q is not part of %s (repos: %v)", *repo, *project, proj.RepoNames())
	}
	m, err := source.NewMock(cfg.Source.Dir)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = time.Now().UTC().Format("20060102-150405")
	}
	// @path references in the body are copied next to the task (see internal/refs).
	b, _, err := refs.Attach(*body, refs.Dir(cfg.Home, store.TaskID(m.Name(), *id)))
	if err != nil {
		return err
	}
	return m.Submit(source.Ticket{ID: *id, Title: *title, Body: b, Project: *project, Repo: *repo})
}

// draftTicket asks Claude for a ticket draft, prints it and, unless yes, asks
// for confirmation on the terminal. ok is false when the operator declines.
func draftTicket(cfg *config.Config, prompt string, yes bool) (tk source.Ticket, ok bool, err error) {
	if prompt == "-" {
		if !yes {
			return tk, false, errors.New("-prompt - reads the request from stdin, so it cannot also ask for confirmation: add -yes")
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return tk, false, err
		}
		prompt = string(b)
	}
	fmt.Fprintln(os.Stderr, "Claude is drafting the ticket…")
	d, err := (&intake.Drafter{Config: cfg}).Draft(context.Background(), prompt)
	if err != nil {
		return tk, false, err
	}
	tk = d.Ticket(prompt)
	repo := tk.Repo
	if repo == "" {
		repo = "(agent chooses)"
	}
	fmt.Printf("project: %s\nrepo:    %s\ntitle:   %s\n\n%s\n", tk.Project, repo, tk.Title, tk.Body)
	if d.Note != "" {
		fmt.Printf("\nnote: %s\n", d.Note)
	}
	if yes {
		return tk, true, nil
	}
	fmt.Fprint(os.Stderr, "\nSubmit this ticket? [y/N] ")
	var answer string
	fmt.Fscanln(os.Stdin, &answer)
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Fprintln(os.Stderr, "not submitted")
		return tk, false, nil
	}
	return tk, true, nil
}

func cmdDecide(verb string, args []string) error {
	c := newFlags(verb)
	comment := c.fs.String("comment", "", "comment passed to the agent")
	task := parseWithTask(c.fs, args)
	if task == "" {
		return fmt.Errorf("usage: taskpilot %s TASK [-comment C]", verb)
	}
	if verb == "reject" && *comment == "" {
		return fmt.Errorf("reject needs -comment so the agent knows what to change")
	}
	_, st, err := c.load()
	if err != nil {
		return err
	}
	req, err := st.PendingRequest(task)
	if err != nil {
		return err
	}
	if req == nil {
		return fmt.Errorf("task %s has no pending approval request", task)
	}
	text, _, err := refs.Attach(*comment, refs.Dir(st.Home, task))
	if err != nil {
		return err
	}
	if err := st.Decide(task, req, verb == "approve", text); err != nil {
		return err
	}
	fmt.Printf("%sd %s\n", verb, req.ID)
	return nil
}

func cmdMessage(args []string) error {
	c := newFlags("message")
	text := c.fs.String("text", "", "message for the agent")
	task := parseWithTask(c.fs, args)
	if task == "" || *text == "" {
		return fmt.Errorf("usage: taskpilot message TASK -text M")
	}
	_, st, err := c.load()
	if err != nil {
		return err
	}
	if !st.Exists(task) {
		return fmt.Errorf("unknown task %s", task)
	}
	msg, _, err := refs.Attach(*text, refs.Dir(st.Home, task))
	if err != nil {
		return err
	}
	return st.SetNudge(task, msg)
}

// parseWithTask parses flags and returns the task id, accepted before or after the flags.
func parseWithTask(fs *flag.FlagSet, args []string) string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fs.Parse(args[1:])
		return args[0]
	}
	fs.Parse(args)
	return fs.Arg(0)
}

func cmdMCP(args []string) error {
	c := newFlags("mcp")
	task := c.fs.String("task", os.Getenv("TASKPILOT_TASK_ID"), "task id")
	c.fs.Parse(args)
	cfg, st, err := c.load()
	if err != nil {
		return err
	}
	if !store.ValidID(*task) || !st.Exists(*task) {
		return fmt.Errorf("unknown task %q", *task)
	}
	srv := mcpserver.NewServer(&mcpserver.Handlers{Store: st, Config: cfg, TaskID: *task,
		Bitbucket: func(ctx context.Context) (*bitbucket.Client, error) { return bitbucketClient(ctx, cfg, "mcp:"+*task) },
		Splunk:    func(ctx context.Context) (*splunk.Client, error) { return splunkClient(ctx, cfg, "mcp:"+*task) },
		ArgoCD:    func(ctx context.Context) (*argocd.Client, error) { return argocdClient(ctx, cfg, "mcp:"+*task) },
		Vault:     func(ctx context.Context) (*vault.Client, error) { return vaultClient(ctx, cfg, "mcp:"+*task) },
		Kafka: func(ctx context.Context) (*kafbat.Client, string, error) {
			cl, err := kafkaClient(ctx, cfg, "mcp:"+*task)
			if err != nil {
				return nil, "", err
			}
			cluster, err := kafkaCluster(ctx, cfg, cl)
			return cl, cluster, err
		},
		ClaudeArtifact: func(ctx context.Context, link, dir string) (*claudeai.Artifact, error) {
			f, err := claudeFetcher(ctx, cfg, "mcp:"+*task)
			if err != nil {
				return nil, err
			}
			return f.Fetch(ctx, link, dir)
		},
		Jenkins: func(ctx context.Context) (*jenkins.Client, error) { return jenkinsClient(ctx, cfg, "mcp:"+*task) },
		Secret:  secretReader(cfg, "mcp:"+*task)})
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}

func cmdHook(args []string) error {
	c := newFlags("hook")
	task := c.fs.String("task", os.Getenv("TASKPILOT_TASK_ID"), "task id")
	format := c.fs.String("format", "", "hook response format: claude, cursor, or empty (auto)")
	c.fs.Parse(args)
	// Fail closed: a broken config must not silently allow gated calls.
	cfg, st, err := c.load()
	if err != nil {
		return hook.Deny(os.Stdout, "taskpilot hook could not load its config ("+err.Error()+"); blocked to be safe")
	}
	pol, err := gate.Compile(cfg.Rules)
	if err != nil {
		return hook.Deny(os.Stdout, "taskpilot hook has an invalid policy ("+err.Error()+"); blocked to be safe")
	}
	var opts hook.Options
	if t, err := st.LoadTask(*task); err == nil && cfg.Graphify != nil && len(cfg.ProjectGraphLines(t.Project)) > 0 {
		opts.Graphify = true
	}
	return hook.RunWith(os.Stdin, os.Stdout, st, *task, pol, *format, opts)
}

// newDaemon builds the scheduler (and its error scanner) the same way for
// `taskpilot daemon` and the daemon embedded in the TUI.
func newDaemon(cfg *config.Config, st *store.Store, exe string, logw io.Writer) (*daemon.Daemon, error) {
	src, err := source.New(cfg.Source)
	if err != nil {
		return nil, err
	}
	pluginDir, err := plugin.Materialize(cfg.Home)
	if err != nil {
		return nil, fmt.Errorf("write plugin: %w", err)
	}
	if _, err := gate.Compile(cfg.Rules); err != nil {
		return nil, err
	}
	d := &daemon.Daemon{
		Config: cfg, Store: st, Source: src,
		Runner: &runner.Runner{Config: cfg, Store: st, PluginDir: pluginDir, Exe: exe},
		Log:    log.New(logw, "", log.LstdFlags),
	}
	d.Scanner = &scan.Scanner{Config: d.CurrentConfig, Store: st, Logf: d.Log.Printf,
		Splunk: func(ctx context.Context) (*splunk.Client, error) {
			return splunkClient(ctx, d.CurrentConfig(), "daemon:scan")
		}, Group: flowGrouper(d.CurrentConfig)}
	d.Indexer = &graphify.Indexer{Config: d.CurrentConfig, Logf: d.Log.Printf}
	return d, nil
}
