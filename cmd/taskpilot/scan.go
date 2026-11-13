package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/scan"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

const scanUsage = `taskpilot scan <command>     (scheduled production error scans: logs_production.scan)

  run PROJECT     scan now (the time since the previous scan, at most max_window)
  regroup PROJECT group the waiting suggestions into failing flows again
  list [PROJECT]  the fix suggestions waiting for you (TUI: tab 3 Logs → Production → 🔔)
`

func cmdScan(args []string) error {
	if len(args) == 0 {
		fmt.Print(scanUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	c := newFlags("scan " + sub)
	pos := c.parse(args)
	cfg, st, err := c.load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	s := &scan.Scanner{Config: func() *config.Config { return cfg }, Store: st,
		Splunk: func(ctx context.Context) (*splunk.Client, error) { return splunkClient(ctx, cfg, "cli:scan") },
		Logf:   func(f string, a ...any) { fmt.Printf(f+"\n", a...) }, Group: flowGrouper(func() *config.Config { return cfg })}
	switch sub {
	case "run":
		if len(pos) != 1 {
			return fmt.Errorf("usage: taskpilot scan run PROJECT")
		}
		_, err := s.Run(ctx, pos[0])
		return err
	case "regroup":
		if len(pos) != 1 {
			return fmt.Errorf("usage: taskpilot scan regroup PROJECT")
		}
		fs, gs, err := s.Regroup(ctx, pos[0])
		if err != nil {
			return err
		}
		fmt.Printf("%d nhóm lỗi đang chờ → %d luồng nhiều nhóm (xem: taskpilot scan list %s)\n", len(gs), len(fs), pos[0])
		return nil
	case "list":
		p := ""
		if len(pos) > 0 {
			p = pos[0]
		}
		fs, err := st.SuggestionFlows(p)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSERVICE\tLẦN QUÉT CUỐI\tTỔNG\tGẦN NHẤT\tTHÔNG ĐIỆP")
		for _, f := range fs {
			if len(f.Members) > 1 {
				w.Flush()
				how := map[string]string{"request_id": "cùng request/giao dịch", "claude": "Claude gom"}[f.How]
				fmt.Printf("\n▸ %s  (%d nhóm, %d lần, %s)\n", f.Title, len(f.Members), f.Total(), how)
				if f.Cause != "" {
					fmt.Printf("  nguyên nhân có thể: %s\n", f.Cause)
				}
			}
			for _, g := range f.Members {
				ind := ""
				if len(f.Members) > 1 {
					ind = "  "
				}
				fmt.Fprintf(w, "%s%d\t%s\t%d\t%d\t%s\t%s\n", ind, g.ID, g.Service, g.LastCount, g.Count,
					g.LastSeen.Local().Format("02/01 15:04"), short(g.Signature, 90))
			}
		}
		return w.Flush()
	}
	fmt.Print(scanUsage)
	return fmt.Errorf("unknown scan command %q", sub)
}

// flowGrouper is the Claude pass of the scan flows, with the current config.
func flowGrouper(cfg func() *config.Config) scan.Grouper {
	return func(ctx context.Context, in scan.GroupInput) ([]scan.Flow, error) {
		c := cfg().Claude
		g := &scan.ClaudeGrouper{Bin: c.Bin, Model: c.Model, SettingSources: c.SettingSources}
		return g.Group(ctx, in)
	}
}
