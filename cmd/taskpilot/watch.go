package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/herdr"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/tui"
)

// cmdWatch shows a background task live in the terminal. Inside herdr the
// pane reports itself as the task's Claude agent, so herdr lists agents the
// daemon runs in the background; the tab closes once the task is done.
func cmdWatch(args []string) error {
	c := newFlags("watch")
	id := parseWithTask(c.fs, args)
	if id == "" {
		return errors.New("usage: taskpilot watch TASK")
	}
	_, st, err := c.load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	var rep tui.WatchReporter = noReport{}
	if hc, ok := herdr.New(); ok && herdr.Inside() && os.Getenv("HERDR_PANE_ID") != "" {
		rep = &herdrReport{c: hc, pane: os.Getenv("HERDR_PANE_ID"), tab: os.Getenv("HERDR_TAB_ID")}
		defer hc.ReleaseAgent(context.Background(), os.Getenv("HERDR_PANE_ID"))
	}
	fmt.Print("\x1b[?25l")       // hide the cursor
	defer fmt.Print("\x1b[?25h") // and bring it back
	err = tui.Watch(ctx, st, id, rep, os.Stdout, func() (int, int) {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			return 100, 40
		}
		return w, h
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type noReport struct{}

func (noReport) Agent(string, string, string) error { return nil }
func (noReport) Title(string) error                 { return nil }
func (noReport) Done() error                        { return nil }

// herdrReport reports the watched task as the pane's agent.
type herdrReport struct {
	c         *herdr.Client
	pane, tab string
}

func (r *herdrReport) Agent(state, message, sessionID string) error {
	return r.c.ReportAgent(context.Background(), r.pane, state, message, sessionID)
}

func (r *herdrReport) Title(title string) error {
	return r.c.ReportTitle(context.Background(), r.pane, title)
}

func (r *herdrReport) Done() error {
	ctx := context.Background()
	_ = r.c.ReleaseAgent(ctx, r.pane)
	if r.tab == "" {
		return nil
	}
	time.Sleep(3 * time.Second) // the final state stays readable a moment
	return r.c.CloseTab(ctx, r.tab)
}

// herdrWatcher opens a herdr tab watching a task when the daemon starts a
// run, unless one is open already; nil outside herdr.
func herdrWatcher(cfg *config.Config, exe string) func(t *store.Task) {
	hc, ok := herdr.New()
	if !ok || !herdr.Inside() || exe == "" {
		return nil
	}
	return func(t *store.Task) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tabs, err := hc.Tabs(ctx)
		if err != nil {
			return
		}
		label := herdr.WatchLabel(t.ID)
		for _, x := range tabs {
			if x.Label == label {
				return
			}
		}
		_, pane, err := hc.CreateTab(ctx, label, t.Workspace)
		if err != nil {
			return
		}
		_ = hc.Run(ctx, pane, herdr.Command(exe, "watch", t.ID, "-config", cfg.Path)+"; exit")
	}
}
