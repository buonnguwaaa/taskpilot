package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func pushFixture(t *testing.T, title string) (Model, *store.Store, *[]hasakiwork.NewTask, *[][2]int64) {
	t.Helper()
	m, st, _ := newTest(t)
	var created []hasakiwork.NewTask
	var assigned [][2]int64
	m.d.HW = fakeHW{created: &created, assigned: &assigned, epics: map[int64]map[hasakiwork.EpicKind]*hasakiwork.Epic{
		8388: {
			hasakiwork.EpicDev:     {Kind: hasakiwork.EpicDev, ID: 14460784, Name: "[EPIC] DEV Sprint 10", MilestoneID: 14460773, Milestone: "Sprint 10: Dynamic planning"},
			hasakiwork.EpicImprove: {Kind: hasakiwork.EpicImprove, ID: 14460782, Name: "[EPIC] Improve Sprint 10", MilestoneID: 14460773, Milestone: "Sprint 10: Dynamic planning"},
		},
	}}
	tk := &store.Task{ID: "mock-7", Title: title, Project: "bom", Runs: 1}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	m.reloadTasks()
	return m, st, &created, &assigned
}

// run executes cmd and feeds its message back, like the bubbletea runtime.
func run(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	tm, _ := m.Update(cmd())
	return tm.(Model)
}

func TestPushTaskToHW(t *testing.T) {
	m, st, created, assigned := pushFixture(t, "[BE] Enhance report latency")
	st.SetCompletion("mock-7", &store.Completion{Summary: "Đã thêm index", CreatedAt: time.Now()})

	m, cmd := press(t, m, "h")
	if m.modal != nil {
		t.Fatal("the form waits for the EPICs")
	}
	m = run(t, m, cmd)
	if m.modal == nil {
		t.Fatalf("form should open once the EPICs are found: %q", m.msg)
	}
	f := m.modal
	if f.value("project") != "bom" || f.value("name") != "[BE] Enhance report latency" || f.value("kind") != "improve" || f.value("note") != "Đã thêm index" {
		t.Fatalf("prefill: project=%q name=%q kind=%q note=%q", f.value("project"), f.value("name"), f.value("kind"), f.value("note"))
	}
	if !strings.Contains(m.View(), "[EPIC] Improve Sprint 10") {
		t.Fatal("the chosen EPIC must be shown")
	}
	m, cmd = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form error: %s", m.modal.err)
	}
	if m2, _ := press(t, m, "h"); m2.modal != nil {
		t.Fatal("double push while in flight")
	}
	m = run(t, m, cmd)

	if len(*created) != 1 {
		t.Fatalf("CreateTask calls = %d", len(*created))
	}
	got := (*created)[0]
	if got.ProjectID != 8388 || got.ParentID != 14460782 || got.MilestoneID != 14460773 || got.PlannedMinutes != 480 ||
		got.Note != "Đã thêm index" || got.DateEnd.Format("2006-01-02") != time.Now().Format("2006-01-02") {
		t.Fatalf("NewTask = %+v", got)
	}
	if len(*assigned) != 1 || (*assigned)[0] != [2]int64{9001, 39324} {
		t.Fatalf("assigned = %v", *assigned)
	}
	if !strings.Contains(m.msg, "HSK-NEW1") || !strings.Contains(m.msg, "giao cho bạn") {
		t.Fatalf("msg = %q", m.msg)
	}
	if l, _ := st.HasakiLink("mock-7"); l == nil || l.Code != "HSK-NEW1" {
		t.Fatalf("link = %+v", l)
	}
	if !strings.Contains(m.tasks.lastBody, "Đã đẩy lên Hasaki Work: HSK-NEW1") {
		t.Fatal("detail should show the Hasaki Work code")
	}
	// Pushing again is allowed, with a warning, and creates a second task.
	m, _ = press(t, m, "h")
	if m.modal == nil || !strings.Contains(m.modal.hint, "đã đẩy lên Hasaki Work: HSK-NEW1") {
		t.Fatalf("re-push must open the form with a warning: modal=%v msg=%q", m.modal != nil, m.msg)
	}
	m, cmd = press(t, m, "ctrl+s")
	m = run(t, m, cmd)
	if len(*created) != 2 || !strings.Contains(m.msg, "HSK-NEW2") || !strings.Contains(m.msg, "thay cho HSK-NEW1") {
		t.Fatalf("re-push: calls=%d msg=%q", len(*created), m.msg)
	}
	if l, _ := st.HasakiLink("mock-7"); l == nil || l.Code != "HSK-NEW2" {
		t.Fatalf("link must point to the new task: %+v", l)
	}
}

func TestPushTaskMissingEpic(t *testing.T) {
	m, _, created, _ := pushFixture(t, "Hỗ trợ issue production đơn bị treo")
	m, cmd := press(t, m, "h")
	m = run(t, m, cmd)
	if m.modal == nil || m.modal.value("kind") != "issue" {
		t.Fatal("issue production task should default to the Issue Production EPIC")
	}
	m, _ = press(t, m, "ctrl+s")
	if m.modal == nil || !strings.Contains(m.modal.err, "EPIC khác") {
		t.Fatal("no open Issue Production EPIC: the form must ask for one")
	}
	if len(*created) != 0 {
		t.Fatal("nothing may be created")
	}
	// Cached: the next h opens the form at once.
	m, _ = press(t, m, "esc")
	if m, _ = press(t, m, "h"); m.modal == nil {
		t.Fatal("cached EPICs: the form opens immediately")
	}
}

func TestPushTaskFromHWNotLoaded(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "hw-1", Source: hwSource, TicketID: "HSK-AAA", Title: "x", Project: "bom"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m, _ = press(t, m, "h")
	// Already on Hasaki Work: h adds subtasks instead; this one is not in the loaded list yet.
	if m.modal != nil || !strings.Contains(m.msg, "bấm n trên task đó") {
		t.Fatalf("modal=%v msg=%q", m.modal != nil, m.msg)
	}
}

func TestParseHWInputs(t *testing.T) {
	for _, v := range []map[string]string{
		{"name": " "},
		{"name": "a", "hours": "-1"},
		{"name": "a", "deadline": "15/11/2026"},
	} {
		if _, err := parseNewHWTask(v, 1); err == nil {
			t.Errorf("%v: want an error", v)
		}
	}
	nt, err := parseNewHWTask(map[string]string{"name": "a", "hours": "1,5", "deadline": ""}, 5)
	if err != nil || nt.PlannedMinutes != 90 || !nt.DateEnd.IsZero() || nt.ProjectID != 5 {
		t.Fatalf("%+v %v", nt, err)
	}
	for in, want := range map[string]int64{"": 0, "123": 123, "#77": 77, "https://work.hasaki.vn/tasks?task_id=14460782": 14460782} {
		if got, err := parseEpicID(in); err != nil || got != want {
			t.Errorf("parseEpicID(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := parseEpicID("abc"); err == nil {
		t.Error("parseEpicID(abc) must fail")
	}
}

func TestGuessEpicKind(t *testing.T) {
	for title, want := range map[string]hasakiwork.EpicKind{
		"[BE] Thêm API export":                      hasakiwork.EpicDev,
		"[Issue Production] Fix mismatching RFID":   hasakiwork.EpicIssue,
		"[BE] Cải tiến import PL schedule":          hasakiwork.EpicImprove,
		"[BUG] [BE] File excel sai ở môi trường QC": hasakiwork.EpicDev,
		"Hotfix lỗi signature invalid":              hasakiwork.EpicIssue,
	} {
		if got := guessEpicKind(&store.Task{Title: title}); got != want {
			t.Errorf("%q → %s, want %s", title, got, want)
		}
	}
}
