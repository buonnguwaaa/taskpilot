package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/worklog"
)

func pushFixture(t *testing.T, title string) (Model, *store.Store, *[]hasakiwork.NewTask, *[][2]int64) {
	t.Helper()
	m, st, _ := newTest(t)
	var created []hasakiwork.NewTask
	var assigned [][2]int64
	var reports []fakeReport
	pushReports = &reports
	m.d.HW = fakeHW{created: &created, assigned: &assigned, reports: &reports, epics: map[int64]map[hasakiwork.EpicKind]*hasakiwork.Epic{
		8388: {
			hasakiwork.EpicDev:     {Kind: hasakiwork.EpicDev, ID: 14460784, Name: "[EPIC] DEV Sprint 10", MilestoneID: 14460773, Milestone: "Sprint 10: Dynamic planning"},
			hasakiwork.EpicImprove: {Kind: hasakiwork.EpicImprove, ID: 14460782, Name: "[EPIC] Improve Sprint 10", MilestoneID: 14460773, Milestone: "Sprint 10: Dynamic planning"},
		},
	}}
	tk := &store.Task{ID: "mock-7", Title: title, Project: "bom", Runs: 1, Body: "Tăng tốc báo cáo staff"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	m.reloadTasks()
	return m, st, &created, &assigned
}

// pushReports are the Report calls of the last pushFixture.
var pushReports *[]fakeReport

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
	if f.value("project") != "bom" || f.value("name") != "[BE] Enhance report latency" || f.value("kind") != "improve" || f.value("note") != "Tăng tốc báo cáo staff" ||
		f.value("result") != "Đã thêm index" || f.value("status") != "2" {
		t.Fatalf("prefill: project=%q name=%q kind=%q note=%q result=%q status=%q", f.value("project"), f.value("name"), f.value("kind"),
			f.value("note"), f.value("result"), f.value("status"))
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
		got.Note != "Tăng tốc báo cáo staff" || got.DateEnd.Format("2006-01-02") != time.Now().Format("2006-01-02") {
		t.Fatalf("NewTask = %+v", got)
	}
	if len(*assigned) != 1 || (*assigned)[0] != [2]int64{9001, 39324} {
		t.Fatalf("assigned = %v", *assigned)
	}
	// Then the work result is written and the task marked done (the task is done here).
	if r := *pushReports; len(r) != 1 || r[0] != (fakeReport{9001, 39324, "Đã thêm index", 480, 2}) {
		t.Fatalf("reports = %+v", r)
	}
	if !strings.Contains(m.msg, "HSK-NEW1") || !strings.Contains(m.msg, "giao cho bạn") || !strings.Contains(m.msg, "đánh hoàn thành") {
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
	// Already on Hasaki Work: h reports on it; its id is unknown here (not dispatched by taskpilot, list not loaded).
	if m.modal != nil || !strings.Contains(m.msg, "không tìm thấy id của HSK-AAA") {
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

// A task that came from Hasaki Work: h creates a finished SUBTASK under it;
// the parent is never reported on.
func TestReportOnSourceTask(t *testing.T) {
	m, st, _ := newTest(t)
	var created []hasakiwork.NewTask
	var assigned [][2]int64
	var reports []fakeReport
	parent := hasakiwork.Task{ID: 14560066, Code: "HSK-SRC", Name: "Chặn gen WO", ProjectID: 8388, MilestoneID: 14460773, DateEnd: "2099-11-27 00:00:00"}
	m.d.HW = fakeHW{tasks: []hasakiwork.Task{parent}, created: &created, assigned: &assigned, reports: &reports}
	tk := &store.Task{ID: "hw-HSK-SRC", Source: hwSource, TicketID: "HSK-SRC", Title: "Chặn gen WO khi SKU inactive", Project: "bom",
		Body: "HSK-SRC (id 14560066) — project BOM (Manufactory)\n\nDescription:\n…"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	st.SetCompletion("hw-HSK-SRC", &store.Completion{Summary: "Đã chặn gen WO khi SKU inactive", CreatedAt: time.Now()})
	st.SetPullRequest("hw-HSK-SRC", &store.PullRequest{Repo: "bom-report", Destination: "release/qc", ID: 286,
		URL: "https://bitbucket.org/hasaki-tech/bom-report/pull-requests/286", CreatedAt: time.Now()})
	m.reloadTasks()

	m, _ = press(t, m, "h")
	f := m.modal
	if f == nil || !strings.Contains(f.title, "Subtask hoàn thành dưới HSK-SRC") || f.value("name") != tk.Title || f.value("status") != "2" ||
		!strings.Contains(f.value("result"), "Đã chặn gen WO") || !strings.Contains(f.value("result"), "pull-requests/286") ||
		!strings.Contains(f.hint, "không bị đổi") {
		t.Fatalf("form: %+v", f)
	}
	m, _ = press(t, m, "tab", "tab") // name → result → giờ thực tế
	m = typeText(m, "2,5")
	m, cmd := press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form error: %s", m.modal.err)
	}
	m = run(t, m, cmd)

	if len(created) != 1 {
		t.Fatalf("created = %+v", created)
	}
	c := created[0]
	if c.ParentID != 14560066 || c.ProjectID != 8388 || c.MilestoneID != 14460773 || c.PlannedMinutes != 150 ||
		c.Name != tk.Title || c.DateEnd.Format("2006-01-02") != "2099-11-27" {
		t.Fatalf("subtask = %+v", c)
	}
	if len(assigned) != 1 || assigned[0] != [2]int64{9001, 39324} {
		t.Fatalf("assigned = %v", assigned)
	}
	if len(reports) != 1 || reports[0] != (fakeReport{9001, 39324, reports[0].result, 150, 2}) {
		t.Fatalf("the report goes on the subtask, never the parent: %+v", reports)
	}
	if l, _ := st.HasakiLink("hw-HSK-SRC"); l == nil || l.Code != "HSK-NEW1" {
		t.Fatalf("link = %+v", l)
	}
	if !strings.Contains(m.msg, "đã tạo subtask HSK-NEW1 dưới HSK-SRC") || !strings.Contains(m.msg, "HSK-SRC giữ nguyên") {
		t.Fatalf("msg = %q", m.msg)
	}
}

func TestParseActualHours(t *testing.T) {
	for in, want := range map[string]int{"": 0, "2": 120, "1,5": 90, "0.25": 15} {
		if got, err := parseActualHours(in); err != nil || got != want {
			t.Errorf("parseActualHours(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"abc", "-1", "0", "500"} {
		if _, err := parseActualHours(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if hoursText(480) != "8" || hoursText(90) != "1,5" || hoursText(0) != "" {
		t.Error("hoursText")
	}
}

// With the drafter, h first asks Claude for the name and the result, then
// opens the form prefilled with them (PR links always kept).
func TestReportDraftedByClaude(t *testing.T) {
	m, st, _ := newTest(t)
	var created []hasakiwork.NewTask
	var assigned [][2]int64
	var reports []fakeReport
	parent := hasakiwork.Task{ID: 14560066, Code: "HSK-SRC", Name: "[Planning MO] Chặn gen WO", ProjectID: 8388, MilestoneID: 1}
	m.d.HW = fakeHW{tasks: []hasakiwork.Task{parent}, created: &created, assigned: &assigned, reports: &reports}
	var asked worklog.Input
	m.d.WorkLog = func(_ context.Context, in worklog.Input) (*worklog.Draft, error) {
		asked = in
		return &worklog.Draft{Name: "[BE] Chặn gen WO khi BOM có SKU inactive", Result: "Thêm kiểm tra SKU inactive ở bom-report."}, nil
	}
	tk := &store.Task{ID: "hw-HSK-SRC", Source: hwSource, TicketID: "HSK-SRC", Title: "Chặn gen WO", Project: "bom",
		Body: "HSK-SRC (id 14560066) — project BOM"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	st.SetPullRequest("hw-HSK-SRC", &store.PullRequest{Repo: "bom-report", Destination: "release/qc", ID: 286,
		URL: "https://bitbucket.org/x/286", CreatedAt: time.Now()})
	m.reloadTasks()

	m, cmd := press(t, m, "h")
	if m.modal != nil || cmd == nil || !strings.Contains(m.msg, "Claude đang soạn") {
		t.Fatalf("h drafts first: modal=%v msg=%q", m.modal != nil, m.msg)
	}
	m = run(t, m, cmd)
	f := m.modal
	if f == nil || f.value("name") != "[BE] Chặn gen WO khi BOM có SKU inactive" ||
		!strings.HasPrefix(f.value("result"), "Thêm kiểm tra SKU inactive") || !strings.Contains(f.value("result"), "https://bitbucket.org/x/286") ||
		!strings.Contains(f.hint, "do Claude soạn") {
		t.Fatalf("form: %+v", f)
	}
	if !asked.Subtask || !strings.Contains(asked.Parent, "HSK-SRC") || len(asked.PRs) != 1 {
		t.Fatalf("drafter input = %+v", asked)
	}
	// Esc then h again: the draft is reused, no second call.
	asked = worklog.Input{}
	m, _ = press(t, m, "esc")
	m, cmd = press(t, m, "h")
	if m.modal == nil || asked.Title != "" {
		t.Fatal("the draft is reused")
	}
	_ = cmd
}

func TestReportDraftFailureFallsBack(t *testing.T) {
	m, st, _ := newTest(t)
	m.d.HW = fakeHW{tasks: []hasakiwork.Task{{ID: 7, Code: "HSK-SRC", ProjectID: 8388}}}
	m.d.WorkLog = func(context.Context, worklog.Input) (*worklog.Draft, error) {
		return nil, errors.New("claude quá thời gian")
	}
	tk := &store.Task{ID: "hw-HSK-SRC", Source: hwSource, TicketID: "HSK-SRC", Title: "Chặn gen WO", Project: "bom", Body: "HSK-SRC (id 7)"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m, cmd := press(t, m, "h")
	m = run(t, m, cmd)
	if m.modal == nil || m.modal.value("name") != "Chặn gen WO" || !strings.Contains(m.modal.hint, "Claude không soạn được") {
		t.Fatalf("fallback form: %+v", m.modal)
	}
}
