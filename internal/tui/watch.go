package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// WatchReporter is where Watch reports the task as an agent (a herdr pane).
type WatchReporter interface {
	// Agent reports the state (herdr idle, working, blocked) and a message.
	Agent(state, message, sessionID string) error
	Title(title string) error
	// Done stops reporting and closes the view (the task is finished).
	Done() error
}

// Watch shows a background task live (title, state, the agent's latest
// activity) and reports it, so a terminal manager lists it as a running
// agent. It returns once the task is done or deleted, or ctx ends. size
// gives the terminal size (width, height).
func Watch(ctx context.Context, st *store.Store, id string, rep WatchReporter, out io.Writer, size func() (int, int)) error {
	var lastView, lastReport, lastTitle string
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		t, err := st.LoadTask(id)
		if errors.Is(err, store.ErrNotFound) || err == nil && t.State == store.StateDone {
			if t != nil {
				fmt.Fprint(out, "\x1b[H\x1b[2J"+stateChip(t.State)+"  "+t.Title+"\n")
			}
			return rep.Done()
		}
		if err != nil {
			return err
		}
		w, h := size()
		w, h = max(40, w), max(8, h)
		acts := activity(st.EventsPath(id), 400)

		if t.Title != lastTitle {
			lastTitle = t.Title
			_ = rep.Title(truncate(t.Title, 60))
		}
		state, msg := watchState(st, t, acts)
		if r := state + "\x00" + msg + "\x00" + t.SessionID; r != lastReport {
			lastReport = r
			_ = rep.Agent(state, msg, t.SessionID)
		}

		var b strings.Builder
		b.WriteString(titleStyle.Render(ansi.Truncate(t.Title, w, "…")) + "\n")
		meta := []string{stateChip(t.State), t.ID}
		if t.Project != "" {
			meta = append(meta, t.Project)
		}
		meta = append(meta, fmt.Sprintf("$%.2f · %d lượt chạy", t.CostUSD, t.Runs))
		b.WriteString(ansi.Truncate(strings.Join(meta, dimStyle.Render(" · ")), w, "…") + "\n")
		b.WriteString(dimStyle.Render(ansi.Truncate("Chỉ xem: duyệt / nhắn / làm tay trong taskpilot (tab Tasks). Tab này tự đóng khi task xong.", w, "…")) + "\n")
		b.WriteString(ruleStyle.Render(strings.Repeat("─", w)) + "\n")
		rows := h - 5
		if len(acts) > rows {
			acts = acts[len(acts)-rows:]
		}
		for _, a := range acts {
			b.WriteString(ansi.Truncate(a, w, "…") + "\n")
		}
		if v := b.String(); v != lastView {
			lastView = v
			fmt.Fprint(out, "\x1b[H\x1b[2J"+v)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// watchState maps a task to the herdr agent state and a one-line message.
func watchState(st *store.Store, t *store.Task, acts []string) (string, string) {
	last := ""
	if len(acts) > 0 {
		last = strings.TrimSpace(ansi.Strip(acts[len(acts)-1]))
	}
	switch t.State {
	case store.StateAwaitApproval:
		if req, _ := st.PendingRequest(t.ID); req != nil {
			return "blocked", truncate("chờ duyệt: "+firstLine(req.Summary), 100)
		}
		return "blocked", "chờ bạn duyệt"
	case store.StateNeedsAttention:
		return "blocked", "cần bạn: " + truncate(firstLine(t.LastResult), 90)
	case store.StateFailed:
		return "blocked", "lỗi: " + truncate(firstLine(t.LastError), 95)
	case store.StateQueued, store.StatePreparing:
		return "working", lookOf(t.State).label
	}
	return "working", truncate(last, 100)
}
