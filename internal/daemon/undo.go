package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// undo takes back the operator input that started the task's latest run
// (stopped already if it was running): the run is marked revoked, the
// message or decision is taken back, and the next run resumes the agent's
// session at the last entry before that run, so the agent never saw it.
// Files the agent changed during the run stay in the worktrees.
func (d *Daemon) undo(t *store.Task) error {
	defer func() { _ = d.Store.ClearUndoRequest(t.ID) }()
	if cur, err := d.Store.LoadTask(t.ID); err == nil {
		t = cur
	}
	refuse := func(why string) error {
		t.LastError = "không thu hồi được: " + why
		d.Log.Printf("%s: %s", t.ID, t.LastError)
		return d.Store.SaveTask(t)
	}
	runs, err := d.Store.RunPrompts(t.ID)
	if err != nil {
		return err
	}
	idx := -1
	for i := len(runs) - 1; i >= 0; i-- {
		if !runs[i].Revoked {
			idx = i
			break
		}
	}
	if idx < 1 {
		return refuse("task chưa có lượt nào do bạn gửi")
	}
	r := runs[idx]
	switch r.Trigger {
	case "operator_message", "approval", "rejection":
	default:
		return refuse(fmt.Sprintf("lượt %d không bắt đầu từ tin nhắn hay quyết định của bạn", r.N))
	}
	if w, err := d.Store.TaskWorker(t.ID); err == nil && w != nil && w.Agent == config.AgentCursor {
		return refuse("task đang chạy bằng Cursor, không tua được session")
	}
	entry := r.ResumeAt // recorded when the run started
	if entry == "" {    // runs of older versions
		entry, err = entryBefore(d.Store.EventsPath(t.ID), runs, idx)
		if err != nil {
			return refuse(err.Error())
		}
	}
	tr, err := d.Store.TakeBackRun(t.ID, runs[idx-1].StartedAt, r)
	if err != nil {
		return err
	}
	if err := d.Store.SetResumeAt(t.ID, entry); err != nil {
		return err
	}
	t.NextPrompt, t.NextTrigger, t.LastError = "", "", ""
	const kept = " Thay đổi agent đã làm trong worktree ở lượt đó (nếu có) vẫn còn."
	if tr.Request != "" {
		t.Transition(store.StateAwaitApproval, fmt.Sprintf("đã thu hồi quyết định %s (lượt %d): yêu cầu lại chờ bạn.%s", tr.Request, r.N, kept))
	} else {
		t.Transition(store.StateNeedsAttention, fmt.Sprintf("đã thu hồi tin nhắn (lượt %d): agent không còn thấy nó; m để nhắn lại.%s", r.N, kept))
	}
	d.Log.Printf("%s: took back run %d (%s); the next run resumes at %s", t.ID, r.N, r.Trigger, entry)
	return d.Store.SaveTask(t)
}

// entryBefore is the last entry of the agent's session before run idx: the
// last top-level message of the previous run in the event log (one segment
// per run, each starting with "system init"; the log may have lost its
// oldest runs, so segments are aligned with runs from the end).
func entryBefore(path string, runs []*store.RunPrompt, idx int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("không đọc được log của agent: %w", err)
	}
	defer f.Close()
	var last []string // last entry of each segment
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type    string  `json:"type"`
			Subtype string  `json:"subtype"`
			UUID    string  `json:"uuid"`
			Parent  *string `json:"parent_tool_use_id"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			last = append(last, "")
		case (ev.Type == "assistant" || ev.Type == "user") && ev.UUID != "" && ev.Parent == nil && len(last) > 0:
			last[len(last)-1] = ev.UUID
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	seg := idx - (len(runs) - len(last)) // the segment of run idx
	if seg < 1 || seg > len(last) {
		return "", errors.New("log của agent không khớp với các lượt chạy")
	}
	if last[seg-1] == "" {
		return "", fmt.Errorf("lượt %d không có tin nhắn nào trong log để tua về", runs[idx-1].N)
	}
	return last[seg-1], nil
}

// lastEntry is the last top-level message of the agent's session in the
// event log ("" when there is none), read from the end of the file.
func lastEntry(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	for chunk := int64(1 << 20); ; chunk *= 4 {
		from := max(0, st.Size()-chunk)
		buf := make([]byte, st.Size()-from)
		if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
			return ""
		}
		lines := strings.Split(string(buf), "\n")
		if from > 0 {
			lines = lines[1:] // partial
		}
		for i := len(lines) - 1; i >= 0; i-- {
			var ev struct {
				Type   string  `json:"type"`
				UUID   string  `json:"uuid"`
				Parent *string `json:"parent_tool_use_id"`
			}
			if json.Unmarshal([]byte(lines[i]), &ev) == nil && (ev.Type == "assistant" || ev.Type == "user") && ev.UUID != "" && ev.Parent == nil {
				return ev.UUID
			}
		}
		if from == 0 {
			return ""
		}
	}
}
