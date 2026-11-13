package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/graphify"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

const graphUsage = `taskpilot graphify <command>    (graphify graphs: graphify in the config; alias: graph)

  build [PROJECT] [-line L] [-force]   build / update now (all projects and lines by default)
  status [PROJECT]                     what each line was built from
  links PROJECT [-line L] [FILTER]     gRPC calls between the repos of a line
  ask PROJECT LINE ACTION ARG…         query "<question>" | explain NODE | path A B | affected NODE
  task TASK [ACTION ARG…]              build / update a task's own graph (its worktrees), then ask it
`

func cmdGraph(args []string) error {
	if len(args) == 0 {
		fmt.Print(graphUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("graphify " + sub)
	line := c.fs.String("line", "", "only this line (prod, qc, …)")
	force := c.fs.Bool("force", false, "rebuild every repo even if its branch did not move")
	pos := c.parse(args)
	cfg, err := config.Load(*c.config)
	if err != nil {
		return err
	}
	if cfg.Graphify == nil {
		return errors.New("graphify is not configured: add `graphify: {}` to the config (defaults: lines prod=release/prod,$base and qc=release/qc)")
	}
	ctx := context.Background()
	projects := cfg.ProjectNames()
	if len(pos) > 0 && sub != "ask" && sub != "task" {
		if _, ok := cfg.Projects[pos[0]]; !ok {
			return fmt.Errorf("unknown project %q", pos[0])
		}
		projects = pos[:1]
	}
	switch sub {
	case "build":
		x := &graphify.Indexer{Config: func() *config.Config { return cfg }, Logf: func(f string, a ...any) { fmt.Printf(f+"\n", a...) }}
		var failed []string
		for _, p := range projects {
			for _, l := range cfg.ProjectGraphLines(p) {
				if *line != "" && l != *line {
					continue
				}
				start := time.Now()
				fmt.Printf("%s/%s…\n", p, l)
				st, err := x.Build(ctx, p, l, *force)
				if err != nil {
					failed = append(failed, p+"/"+l)
					fmt.Printf("  error: %v\n", err)
					continue
				}
				fmt.Printf("  %d nodes, %d gRPC edges between repos (%s)\n", st.Nodes, st.Links, time.Since(start).Round(time.Second))
			}
		}
		fmt.Print("\n")
		for _, p := range projects {
			fmt.Printf("%s\n%s", p, indent(graphify.Summary(cfg, p)))
		}
		if len(failed) > 0 {
			return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
		}
		return nil
	case "status":
		for _, p := range projects {
			fmt.Printf("%s\n%s", p, indent(graphify.Summary(cfg, p)))
		}
		return nil
	case "links":
		if len(pos) < 1 {
			return errors.New("usage: taskpilot graphify links PROJECT [-line L] [FILTER]")
		}
		l := *line
		if l == "" {
			l = "prod"
		}
		ls, err := graphify.ReadLinks(cfg.Graphify.Dir, pos[0], l)
		if err != nil {
			return fmt.Errorf("no links for %s/%s: run taskpilot graphify build %s -line %s", pos[0], l, pos[0], l)
		}
		filter := strings.Join(pos[1:], " ")
		fmt.Print(graphify.FormatLinks(ls, filter, 200))
		return nil
	case "task":
		if len(pos) < 1 {
			return errors.New("usage: taskpilot graphify task TASK [ACTION ARG…]")
		}
		st, err := store.New(cfg.Home)
		if err != nil {
			return err
		}
		t, err := st.LoadTask(pos[0])
		if err != nil {
			return err
		}
		start := time.Now()
		ok, err := graphify.BuildTask(ctx, cfg, t.Project, t.ID, t.Workspace, func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("task %s has no repo open: its graph would be the prod line's", t.ID)
		}
		fmt.Printf("task graph of %s (%s) up to date in %s: %s\n", t.ID, strings.Join(workspace.OpenedRepos(t.Workspace), ", "),
			time.Since(start).Round(time.Millisecond), graphify.TaskGraphPath(cfg.Graphify.Dir, t.ID))
		if len(pos) < 3 {
			return nil
		}
		out, err := graphify.QueryGraph(ctx, cfg.Graphify.Bin, graphify.TaskGraphPath(cfg.Graphify.Dir, t.ID), pos[1], pos[2:], 0)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	case "ask":
		if len(pos) < 4 {
			return errors.New("usage: taskpilot graphify ask PROJECT LINE query|explain|path|affected ARG…")
		}
		out, err := graphify.Query(ctx, cfg.Graphify, pos[0], pos[1], pos[2], pos[3:], 0)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	fmt.Print(graphUsage)
	return fmt.Errorf("unknown graph command %q", sub)
}

func indent(s string) string {
	if s == "" {
		return "  (no line)\n"
	}
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ") + "\n"
}
