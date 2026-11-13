package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/worklog"
)

func TestTaskDetailScrollChatAndAssessment(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-C", Title: "Sửa lỗi prod payment: timeout", Project: "bom", Body: strings.Repeat("dòng mô tả\n", 80), Runs: 2, LastResult: strings.Repeat("kết quả dài\n", 80)}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)
	// Two runs: the plan, then the work after the operator's message.
	t0 := time.Now().Add(-time.Hour)
	for i, trig := range []string{"initial", "operator_message"} {
		if _, err := st.DB.Exec(`INSERT INTO runs (task_id, n, session_id, resume, trigger, prompt, model, effort, started_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, "mock-C", i+1, "s", i, trig, "prompt "+trig, "opus", "high",
			t0.Add(time.Duration(i)*30*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	ev := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Mình đã đọc ticket, kế hoạch:\n1. tìm caller"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rg timeout"}}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.5}`,
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Đã sửa theo góp ý của bạn"}]}}`,
	}
	os.MkdirAll(filepath.Dir(st.EventsPath("mock-C")), 0o755)
	os.WriteFile(st.EventsPath("mock-C"), []byte(strings.Join(ev, "\n")+"\n"), 0o644)
	st.DB.Exec(`INSERT INTO operator_messages (task_id, message, created_at, consumed_at) VALUES (?,?,?,?)`,
		"mock-C", "dùng retry có backoff", t0.Add(20*time.Minute).UTC().Format(time.RFC3339Nano), t0.Add(30*time.Minute).UTC().Format(time.RFC3339Nano))
	m.reloadTasks()

	v := m.View()
	if !strings.Contains(v, "chưa đánh giá") || !strings.Contains(v, "Sửa lỗi") || !strings.Contains(v, "đoán từ cách tạo task") {
		t.Fatalf("no assessment yet, kind guessed:\n%s", v)
	}
	// Claude's estimate shows until the agent assesses, and never replaces it.
	st.SetAssessment("mock-C", &store.Assessment{Level: "light", Kind: "config", Reason: "đoán", SetBy: "estimate"})
	m.reloadTasks()
	m.refreshTaskDetail(true)
	if v := m.View(); !strings.Contains(v, "● Nhẹ") || !strings.Contains(v, "Claude ước lượng") {
		t.Fatalf("estimate:\n%s", v)
	}
	st.SetAssessment("mock-C", &store.Assessment{Level: "hard", Kind: "bugfix", Reason: "payment, nhiều service"})
	st.SetAssessment("mock-C", &store.Assessment{Level: "light", Reason: "x", SetBy: "estimate"})
	m.reloadTasks()
	m.refreshTaskDetail(true)
	if v := m.View(); !strings.Contains(v, "● Khó") || !strings.Contains(v, "agent đánh giá") || !strings.Contains(v, "payment, nhiều service") {
		t.Fatalf("assessment:\n%s", v)
	}

	// Enter: the keys scroll the detail.
	m, _ = press(t, m, "enter")
	if !m.tasks.focus {
		t.Fatal("enter focuses the detail")
	}
	m, _ = press(t, m, "j", "j", "j")
	if m.tasks.vp.YOffset != 3 {
		t.Fatalf("j scrolls: offset %d", m.tasks.vp.YOffset)
	}
	m, _ = press(t, m, "esc")
	if m.tasks.focus {
		t.Fatal("esc gives the keys back to the list")
	}

	// t: the conversation, at its end.
	m, _ = press(t, m, "t")
	body := ansi.Strip(m.tasks.lastBody)
	for _, want := range []string{"Lượt 1 · bắt đầu", "Mình đã đọc ticket", "→ Bash rg timeout", "✉ Bạn nhắn agent", "dùng retry có backoff",
		"Lượt 2 · sau tin nhắn của bạn", "Đã sửa theo góp ý của bạn", "opus/high"} {
		if !strings.Contains(body, want) {
			t.Fatalf("conversation lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "dùng retry") > strings.Index(body, "Lượt 2") || strings.Index(body, "dùng retry") < strings.Index(body, "Mình đã đọc") {
		t.Fatalf("the message sits between the runs:\n%s", body)
	}
	if !m.tasks.vp.AtBottom() {
		t.Fatal("the conversation opens at its end")
	}

}

func TestAutoAssessUnassessedTasks(t *testing.T) {
	m, st, _ := newTest(t)
	for _, id := range []string{"mock-1", "mock-2"} {
		tk := &store.Task{ID: id, Title: "task " + id, Project: "bom", Body: "làm gì đó"}
		tk.Transition(store.StateDone, "")
		st.SaveTask(tk)
	}
	st.SetAssessment("mock-2", &store.Assessment{Level: "hard", Kind: "feature"})
	var asked []string
	m.d.Assess = func(_ context.Context, in worklog.AssessInput) (*worklog.Assessment, error) {
		asked = append(asked, in.Title)
		return &worklog.Assessment{Level: "standard", Kind: "feature", Reason: "một repo"}, nil
	}
	m.reloadTasks()
	m, cmd := m.assessNext()
	if cmd == nil || !m.tasks.assessing {
		t.Fatal("an unassessed task is estimated")
	}
	mm, _ := m.Update(cmd())
	m = mm.(Model)
	if len(asked) != 1 || asked[0] != "task mock-1" {
		t.Fatalf("asked = %v", asked)
	}
	if a, _ := st.TaskAssessment("mock-1"); a == nil || a.Level != "standard" || a.SetBy != "estimate" {
		t.Fatalf("stored = %+v", a)
	}
	if _, cmd := m.assessNext(); cmd != nil {
		t.Fatal("nothing left to estimate")
	}
}

func TestNewTaskWithoutTitle(t *testing.T) {
	m, _, inbox := newTest(t)
	m, _ = press(t, m, "n")
	if m.modal == nil {
		t.Fatal("n opens the form")
	}
	m, _ = press(t, m, "tab", "tab", "tab") // project bom, repo, title left empty → body
	m = typeText(m, "BOM lưu version ở bảng nào?")
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("a task without title is accepted: %s", m.modal.err)
	}
	if tk := readTicket(t, inbox); tk.Title != "BOM lưu version ở bảng nào?" || tk.Project != "bom" {
		t.Fatalf("ticket = %+v", tk)
	}
}
