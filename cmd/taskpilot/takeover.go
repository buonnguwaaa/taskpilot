package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// cmdTakeover lets the operator drive a task's Claude Code session by hand:
// it marks the task as taken over (the daemon leaves it alone), runs
// `claude --resume` in the task workspace, and clears the mark on exit.
func cmdTakeover(args []string) error {
	c := newFlags("takeover")
	id := parseWithTask(c.fs, args)
	if id == "" {
		return errors.New("usage: taskpilot takeover TASK")
	}
	cfg, st, err := c.load()
	if err != nil {
		return err
	}
	return takeover(st, cfg.Claude.Bin, id)
}

func takeover(st *store.Store, claudeBin, id string) error {
	t, err := st.LoadTask(id)
	if err != nil {
		return fmt.Errorf("unknown task %s: %w", id, err)
	}
	if t.State == store.StateRunning || t.State == store.StatePreparing {
		return fmt.Errorf("task %s is running; wait until it stops at a gate", id)
	}
	if t.Workspace == "" || t.Runs == 0 {
		return fmt.Errorf("task %s has no session yet", id)
	}
	if st.TakenOver(id) {
		return fmt.Errorf("task %s is already being driven by hand in another pane", id)
	}
	if err := st.SetTakeover(id, true); err != nil {
		return err
	}
	defer st.SetTakeover(id, false)

	// Ctrl+C belongs to claude. Catch (not ignore) SIGINT so the child still
	// receives it with default handling; on SIGHUP/SIGTERM (pane closed) the
	// deferred cleanup still runs once claude exits.
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		for range sig {
		}
	}()

	fmt.Printf("taskpilot: làm tay task %s (%s)\n", id, t.Title)
	fmt.Println("Daemon sẽ không chạy task này cho tới khi bạn thoát claude (/exit hoặc Ctrl+D).")
	cmd := exec.Command(claudeBin, "--resume", t.SessionID)
	cmd.Dir = t.Workspace
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := cmd.Run()
	fmt.Printf("\ntaskpilot: đã trả task %s cho daemon. Muốn agent chạy tiếp: bấm m trong TUI.\n", id)
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return nil // claude's own exit status is not taskpilot's error
	}
	return runErr
}
