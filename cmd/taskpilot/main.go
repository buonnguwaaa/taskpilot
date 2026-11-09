// Command taskpilot runs ticket-driven Claude Code sessions with human approval gates.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/daemon"
	"github.com/buonnguwaaa/taskpilot/internal/gate"
	"github.com/buonnguwaaa/taskpilot/internal/hook"
	"github.com/buonnguwaaa/taskpilot/internal/mcpserver"
	"github.com/buonnguwaaa/taskpilot/internal/plugin"
	"github.com/buonnguwaaa/taskpilot/internal/runner"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
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
  taskpilot approve TASK [-comment C]           approve the pending request
  taskpilot reject TASK -comment C              reject the pending request
  taskpilot message TASK -text M                resume a stopped task with a message
  taskpilot takeover TASK                       drive the task's Claude session by hand (daemon leaves it alone)
  taskpilot db [stats|tasks|runs TASK|sql "SELECT …"]   inspect the database (read-only)
  taskpilot migrate-files                       import tasks of the old file-based store (also automatic on start)
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

Argo CD (QC):
  taskpilot auth argocd [-token]                log in (Chrome profile) or paste a token
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
	case "migrate-files":
		err = cmdMigrateFiles(args)
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
	src, err := source.New(cfg.Source)
	if err != nil {
		return err
	}
	pluginDir, err := plugin.Materialize(cfg.Home)
	if err != nil {
		return fmt.Errorf("write plugin: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if _, err := gate.Compile(cfg.Rules); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := &daemon.Daemon{
		Config: cfg,
		Store:  st,
		Source: src,
		Runner: &runner.Runner{Config: cfg, Store: st, PluginDir: pluginDir, Exe: exe},
		Log:    log.New(os.Stderr, "", log.LstdFlags),
	}
	if len(cfg.Projects) == 0 {
		return fmt.Errorf("no projects configured in %s", cfg.Path)
	}
	unlock, err := daemon.Lock(cfg.Home)
	if errors.Is(err, daemon.ErrLocked) {
		if oerr := daemon.CheckOwner(cfg.Home); oerr != nil {
			return oerr
		}
	}
	if err != nil {
		return fmt.Errorf("%w (close the TUI, or the other `taskpilot daemon`)", err)
	}
	defer unlock()
	if ids, err := st.ImportLegacy(); err != nil {
		return fmt.Errorf("import file-based tasks: %w", err)
	} else if len(ids) > 0 {
		log.Printf("imported %d file-based task(s) into the database: %s", len(ids), strings.Join(ids, ", "))
	}
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
	c.fs.Parse(args)
	cfg, _, err := c.load()
	if err != nil {
		return err
	}
	if cfg.Source.Type != "mock" {
		return fmt.Errorf("submit only works with the mock source (configured: %s)", cfg.Source.Type)
	}
	if *project == "" || *title == "" {
		return fmt.Errorf("-project and -title are required")
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
	return m.Submit(source.Ticket{ID: *id, Title: *title, Body: *body, Project: *project, Repo: *repo})
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
	if err := st.Decide(task, req, verb == "approve", *comment); err != nil {
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
	return st.SetNudge(task, *text)
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
		ArgoCD:    func(ctx context.Context) (*argocd.Client, error) { return argocdClient(ctx, cfg, "mcp:"+*task) }})
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}

func cmdHook(args []string) error {
	c := newFlags("hook")
	task := c.fs.String("task", os.Getenv("TASKPILOT_TASK_ID"), "task id")
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
	return hook.Run(os.Stdin, os.Stdout, st, *task, pol)
}
