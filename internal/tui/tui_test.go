package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/intake"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splgen"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

type fakeHW struct {
	tasks    []hasakiwork.Task
	created  *[]hasakiwork.NewTask // CreateTask calls, when set
	assigned *[][2]int64           // AssignStaff calls (task, staff)
	epics    map[int64]map[hasakiwork.EpicKind]*hasakiwork.Epic
}

func (f fakeHW) Profile(context.Context) (*hasakiwork.Profile, error) {
	return &hasakiwork.Profile{ID: 43149, StaffID: 39324}, nil
}

func (f fakeHW) CurrentEpics(_ context.Context, prid int64, _ []hasakiwork.Task) (map[hasakiwork.EpicKind]*hasakiwork.Epic, error) {
	return f.epics[prid], nil
}

func (f fakeHW) AssignStaff(_ context.Context, task int64, staff ...int64) error {
	if f.assigned == nil {
		return errors.New("AssignStaff not expected")
	}
	for _, s := range staff {
		*f.assigned = append(*f.assigned, [2]int64{task, s})
	}
	return nil
}

func (f fakeHW) CreateTask(_ context.Context, t hasakiwork.NewTask) (*hasakiwork.Created, error) {
	if f.created == nil {
		return nil, errors.New("CreateTask not expected")
	}
	*f.created = append(*f.created, t)
	return &hasakiwork.Created{ID: 9000 + int64(len(*f.created)), Code: fmt.Sprintf("HSK-NEW%d", len(*f.created))}, nil
}

func (f fakeHW) MyTasks(context.Context, string) ([]hasakiwork.Task, error) { return f.tasks, nil }
func (f fakeHW) Task(_ context.Context, id int64) (*hasakiwork.Task, error) {
	for _, t := range f.tasks {
		if t.ID == id {
			t.Note = "Mô tả của task " + t.Code
			return &t, nil
		}
	}
	return nil, os.ErrNotExist
}

func testConfig() *config.Config {
	return &config.Config{
		Path: "/tmp/tp.yaml",
		Projects: map[string]config.Project{
			"bom": {Name: "BOM", HasakiProjectID: 8388, Repos: map[string]config.Repo{"bom-api": {}, "bom-worker": {}}},
			"plm": {Name: "PLM", HasakiProjectID: 7264, Repos: map[string]config.Repo{"hsk-pdp-backend": {}}},
		},
	}
}

func newTest(t *testing.T) (Model, *store.Store, *source.Mock) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inbox, _ := source.NewMock(t.TempDir())
	bom := &hasakiwork.Ref{ID: 8388, Name: "BOM (Manufactory)"}
	hw := fakeHW{tasks: []hasakiwork.Task{
		{ID: 1, Code: "HSK-AAA", Name: "Sửa logic MO", ProjectID: 8388, Project: bom},
		{ID: 2, Code: "HSK-BBB", Name: "Task ngoài phạm vi", ProjectID: 9999, Project: &hasakiwork.Ref{Name: "Khác"}},
	}}
	m := New(Deps{Config: testConfig(), Store: st, HW: hw, Inbox: inbox,
		OpenURL: func(string) error { return nil }, Copy: func(string) error { return nil }})
	m.width, m.height = 140, 40
	m.resize()
	return m, st, inbox
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "f2":
		return tea.KeyMsg{Type: tea.KeyF2}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(t *testing.T, m Model, keys ...string) (Model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	var tm tea.Model = m
	for _, k := range keys {
		tm, cmd = tm.Update(key(k))
	}
	return tm.(Model), cmd
}

func typeText(m Model, s string) Model {
	var tm tea.Model = m
	for _, r := range s {
		tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return tm.(Model)
}

// drain runs async commands (Hasaki Work loads) and feeds their messages back.
func drain(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil && i < 10; i++ {
		msg := cmd()
		switch msg.(type) {
		case hwTasksMsg, hwDetailMsg, cmdOutputMsg:
		default:
			return m
		}
		var tm tea.Model
		tm, cmd = m.Update(msg)
		m = tm.(Model)
	}
	return m
}

func click(m Model, x, y int) (Model, tea.Cmd) {
	tm, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	return tm.(Model), cmd
}

func readTicket(t *testing.T, inbox *source.Mock) source.Ticket {
	t.Helper()
	tickets, err := inbox.Fetch(context.Background())
	if err != nil || len(tickets) != 1 {
		t.Fatalf("inbox = %v %v", tickets, err)
	}
	return tickets[0]
}

func TestApproveAndRejectFromTasksTab(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "hw-HSK-AAA", Title: "Sửa logic MO", Project: "bom", Runs: 1}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)
	st.AddRequest(tk.ID, "plan", "Sửa ở bom-api", "chi tiết plan dài")
	m.reloadTasks()
	if v := m.View(); !strings.Contains(v, "Chờ bạn duyệt: plan-1") || !strings.Contains(v, "1 Tasks (1 cần bạn)") {
		t.Fatalf("view:\n%s", v)
	}
	m, _ = press(t, m, "x")
	if m.modal == nil {
		t.Fatal("x should open the reject form")
	}
	m, _ = press(t, m, "ctrl+s")
	if m.modal == nil || !strings.Contains(m.modal.err, "không được để trống") {
		t.Fatal("empty reject comment must be refused")
	}
	m = typeText(m, "thiếu test")
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form should close, err=%q", m.modal.err)
	}
	decs, _ := st.Decisions(tk.ID)
	if d := decs["plan-1"]; d == nil || d.Approved || d.Comment != "thiếu test" {
		t.Fatalf("decision = %+v", d)
	}
	st.AddRequest(tk.ID, "plan", "plan v2", "")
	m.reloadTasks()
	m, _ = press(t, m, "a")
	if ok, _ := st.GateApproved(tk.ID, "plan"); !ok {
		t.Fatal("a should approve")
	}
}

func TestDispatchHasakiTaskWithKeysAndMouse(t *testing.T) {
	m, st, inbox := newTest(t)
	x := len([]rune(m.tabLabel(tabTasks))) + 3
	m, cmd := click(m, x, 0)
	if m.tab != tabHasaki {
		t.Fatalf("clicking the header should switch tab, tab=%d", m.tab)
	}
	m = drain(t, m, cmd)
	if len(m.hw.list) != 2 || !strings.Contains(m.View(), "Mô tả của task HSK-AAA") {
		t.Fatalf("list/detail not loaded:\n%s", m.View())
	}
	m, cmd = press(t, m, "j")
	m = drain(t, m, cmd)
	m, _ = press(t, m, "enter")
	if m.modal != nil || !strings.Contains(m.msg, "chưa có trong config") {
		t.Fatalf("out-of-scope task: modal=%v msg=%q", m.modal != nil, m.msg)
	}
	// Rows start at y=3: header (0), box border (1), box title (2).
	m, cmd = click(m, 5, 3)
	m = drain(t, m, cmd)
	if m.hw.cursor != 0 {
		t.Fatalf("click should select row 0, cursor=%d", m.hw.cursor)
	}
	m, _ = press(t, m, "enter")
	if m.modal == nil || m.modal.value("project") != "bom" {
		t.Fatal("dispatch form should open with the project preselected from the task's scope")
	}
	m, _ = press(t, m, "tab", "right") // repo: agent tự chọn → bom-api
	m, _ = press(t, m, "tab")
	m = typeText(m, "ưu tiên sửa backend")
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form error: %s", m.modal.err)
	}
	tk := readTicket(t, inbox)
	if tk.ID != "HSK-AAA" || tk.Source != "hw" || tk.Project != "bom" || tk.Repo != "bom-api" ||
		!strings.Contains(tk.Body, "Mô tả của task HSK-AAA") || !strings.Contains(tk.Body, "ưu tiên sửa backend") {
		t.Fatalf("ticket = %+v", tk)
	}
	created := &store.Task{ID: "hw-HSK-AAA", Title: "Sửa logic MO", Project: "bom"}
	created.Transition(store.StateQueued, "")
	st.SaveTask(created)
	m, _ = press(t, m, "enter")
	if m.tab != tabTasks || m.tasks.current() == nil || m.tasks.current().ID != "hw-HSK-AAA" {
		t.Fatalf("should jump to the existing task, tab=%d", m.tab)
	}
}

func TestNewTaskForm(t *testing.T) {
	m, _, inbox := newTest(t)
	m, _ = press(t, m, "n")
	if m.modal == nil {
		t.Fatal("n should open the new-task form")
	}
	m, _ = press(t, m, "right") // project: bom → plm (sorted)
	m, _ = press(t, m, "tab", "tab")
	m = typeText(m, "Thêm API export")
	m, _ = press(t, m, "tab")
	m = typeText(m, "AC: trả về CSV")
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form error: %s", m.modal.err)
	}
	tk := readTicket(t, inbox)
	if tk.Project != "plm" || tk.Title != "Thêm API export" || tk.Body != "AC: trả về CSV" || tk.Repo != "" {
		t.Fatalf("ticket = %+v", tk)
	}
}

func TestPendingPRKey(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-3", Title: "x", Runs: 2}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	st.SetPullRequest("mock-3", &store.PullRequest{Repo: "bom-api", Destination: "release/qc", Title: "fix: a", Description: "mô tả", Pending: true,
		URL: "https://bitbucket.org/ws/bom-api/pull-requests/new?dest=release%2Fqc&source=tp%2Fmock-3"})
	st.SetPullRequest("mock-3", &store.PullRequest{Repo: "bom-frontend", Destination: "release/qc", ID: 9, URL: "https://bitbucket.org/ws/bom-frontend/pull-requests/9"})
	var opened, copied []string
	m.d.OpenURL = func(u string) error { opened = append(opened, u); return nil }
	m.d.Copy = func(s string) error { copied = append(copied, s); return nil }
	m.reloadTasks()
	if !strings.Contains(m.View(), "chưa tạo: bấm p") {
		t.Fatalf("pending PR not shown:\n%s", m.View())
	}
	m, _ = press(t, m, "p", "p")
	if len(opened) != 2 || !strings.Contains(opened[0], "pull-requests/new") || !strings.HasSuffix(opened[1], "/9") {
		t.Fatalf("opened = %v", opened)
	}
	if len(copied) != 1 || copied[0] != "fix: a\n\nmô tả" {
		t.Fatalf("copied = %q", copied)
	}
}

func TestCommandLineAndQuitConfirm(t *testing.T) {
	m, _, _ := newTest(t)
	m, _ = press(t, m, ":")
	if !m.palette {
		t.Fatal(": should open the command line")
	}
	m = typeText(m, "status")
	m, _ = press(t, m, "enter")
	if m.tab != tabStatus || m.palette {
		t.Fatalf("`status` should switch tab, tab=%d", m.tab)
	}
	m, _ = press(t, m, ":")
	m = typeText(m, "daemon")
	m, _ = press(t, m, "enter")
	if !strings.Contains(m.msg, "không chạy được bên trong TUI") {
		t.Fatalf("msg = %q", m.msg)
	}
	if got := splitArgs(`hw show 1 -x "a b" 'c d'`); strings.Join(got, "|") != "hw|show|1|-x|a b|c d" {
		t.Fatalf("splitArgs = %q", got)
	}

	m.d.Daemon = DaemonInfo{Embedded: true, InFlight: func() int { return 2 }}
	m, cmd := press(t, m, "q")
	if m.conf == nil || cmd != nil {
		t.Fatal("quitting with running tasks must ask for confirmation")
	}
	m, _ = press(t, m, "n")
	if m.conf != nil {
		t.Fatal("n should cancel")
	}
	_, cmd = press(t, m, "q", "y")
	if cmd == nil {
		t.Fatal("y should quit")
	}
}

func TestMouseSelectsTaskRow(t *testing.T) {
	m, st, _ := newTest(t)
	for _, id := range []string{"a", "b", "c"} {
		tk := &store.Task{ID: "mock-" + id, Title: id}
		tk.Transition(store.StateQueued, "")
		st.SaveTask(tk)
	}
	m.reloadTasks()
	m, _ = click(m, 3, 6) // y=3: "Đang làm" header, tasks at y=4..6
	if c := m.tasks.current(); c == nil || c.ID != m.tasks.list[2].ID {
		t.Fatalf("clicked row 2, cursor=%d", m.tasks.cursor)
	}
}

func TestHWTicketBody(t *testing.T) {
	body := hwTicketBody(&hasakiwork.Task{ID: 7, Code: "HSK-1", Note: "  làm X  ", Project: &hasakiwork.Ref{Name: "BOM"},
		Milestone: &hasakiwork.Ref{Name: "Sprint 10"}}, "chú ý Y")
	for _, want := range []string{"HSK-1 (id 7) — project BOM", "Sprint: Sprint 10", "Description:\nlàm X", "Operator notes:\nchú ý Y"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func logsModel(t *testing.T, srvURL string) (Model, *[]string) {
	t.Helper()
	m, _, _ := newTest(t)
	cfg := m.d.Config
	cfg.Splunk = config.Splunk{URL: srvURL, DefaultEarliest: "-1h", Timeout: time.Minute}
	p := cfg.Projects["bom"]
	p.LogsProduction = &config.ProdLogs{Filter: "kubernetes.namespace_name=bom-prod", Knowledge: "bom.md",
		Services: []string{"bom-api", "bom-worker"},
		Queries: []config.LogQuery{
			{Name: "Số lỗi", SPL: "{filter} level=error | stats count by kubernetes.container_name"},
			{Name: "Lỗi của một service", SPL: "{filter} kubernetes.container_name={service} level=error \"{text}\"", Earliest: "-4h"},
		}}
	cfg.Projects["bom"] = p
	plm := cfg.Projects["plm"]
	plm.NoLogs = true
	plm.LogsProduction = &config.ProdLogs{Queries: []config.LogQuery{{Name: "x", SPL: "x"}}}
	cfg.Projects["plm"] = plm
	var sent []string
	m.d.Splunk = func(context.Context) (*splunk.Client, error) {
		return &splunk.Client{BaseURL: srvURL, Token: "t", Poll: time.Millisecond}, nil
	}
	return m, &sent
}

func TestLogsProductionTab(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			r.ParseForm()
			sent = append(sent, r.Form.Get("search")+" @"+r.Form.Get("earliest_time")+"→"+r.Form.Get("latest_time"))
			w.Write([]byte(`{"sid":"1"}`))
		case strings.HasSuffix(r.URL.Path, "/results"):
			w.Write([]byte(`{"results":[{"kubernetes.container_name":"bom-api","count":"3"}]}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Write([]byte(`{"results":[{"_time":"2026-11-09T10:00:00.000+07:00","_raw":"{\"log\":\"{\\\"level\\\":\\\"error\\\",\\\"message\\\":\\\"boom\\\"}\",\"kubernetes\":{\"container_name\":\"bom-api\"}}"}]}`))
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"entry":[{"content":{"isDone":true}}]}`))
		}
	}))
	defer srv.Close()
	m, _ := logsModel(t, srv.URL)

	m, _ = press(t, m, "3", "e") // Logs opens on QC; e switches to Production
	if m.tab != tabLogs || m.logs.env != envProd || len(m.logProjects()) != 1 || !strings.Contains(m.View(), "Số lỗi") || !strings.Contains(m.View(), "Nhóm lỗi") {
		t.Fatalf("logs tab (plm must be hidden: no_logs):\n%s", m.View())
	}
	// A saved stats search runs with the prod filter; its rows come from /results.
	m, cmd := press(t, m, "j", "j", "enter") // past the grouping and the investigation entries
	m = drainLogs(t, m, cmd)
	if len(sent) != 1 || sent[0] != "search kubernetes.namespace_name=bom-prod level=error | stats count by kubernetes.container_name @-1h→now" {
		t.Fatalf("sent = %q", sent)
	}
	if v := m.View(); !strings.Contains(v, "kubernetes.container_name  count") || !strings.Contains(v, "bom-api") {
		t.Fatalf("stats rows not shown as a table:\n%s", v)
	}
	// A saved search with parameters asks for them (service select + text), then runs with its own range.
	m, _ = press(t, m, "j", "enter")
	if m.modal == nil {
		t.Fatal("parameter form expected")
	}
	m, _ = press(t, m, "right", "tab") // service bom-api → bom-worker
	m = typeText(m, "MO-77")
	var tm tea.Model
	tm, cmd = m.Update(key("ctrl+s"))
	m = drainLogs(t, tm.(Model), cmd)
	if len(sent) != 2 || sent[1] != `search kubernetes.namespace_name=bom-prod kubernetes.container_name=bom-worker level=error "MO-77" @-4h→now` {
		t.Fatalf("sent = %q", sent)
	}
	if !strings.Contains(m.View(), "boom") {
		t.Fatalf("events not shown:\n%s", m.View())
	}
	// A preset overrides the saved range: t t t → 15m → 1h → 4h … "Hôm qua" has an end.
	for m.presetLabel() != "Hôm qua" {
		m, _ = press(t, m, "t")
	}
	m, cmd = press(t, m, "r")
	m = drainLogs(t, m, cmd)
	if !strings.HasSuffix(sent[2], "@-1d@d→@d") {
		t.Fatalf("yesterday preset: %q", sent[2])
	}
	// The builder writes the SPL; a hand-written destructive one is refused locally.
	m, _ = press(t, m, "/")
	if m.modal == nil || m.modal.title == "" {
		t.Fatal("builder form expected")
	}
	m, _ = press(t, m, "right")      // service: all → bom-api
	m, _ = press(t, m, "tab", "tab") // level stays error; → text
	m = typeText(m, "MO-77")
	tm, cmd = m.Update(key("ctrl+s"))
	m = drainLogs(t, tm.(Model), cmd)
	if want := `search (kubernetes.namespace_name=bom-prod) (kubernetes.container_name=bom-api (level=error OR level=fatal OR level=panic OR level=dpanic) "MO-77")`; !strings.HasPrefix(sent[3], want) {
		t.Fatalf("built SPL = %q", sent[3])
	}
	m, _ = press(t, m, "/")
	for m.modal.fields[m.modal.focus].key != "spl" {
		m, _ = press(t, m, "tab")
	}
	m.modal.fields[m.modal.focus].area.SetValue("x | outputlookup evil.csv")
	m, _ = press(t, m, "ctrl+s")
	if m.modal == nil || !strings.Contains(m.modal.err, "outputlookup") || len(sent) != 4 {
		t.Fatalf("destructive search must be refused locally (err=%q, sent=%d)", m.modal.err, len(sent))
	}
	m, _ = press(t, m, "esc")
	// i shows the embedded knowledge file.
	m, _ = press(t, m, "i")
	if m.output == nil || !strings.Contains(m.output.vp.View(), "bom_request_logs") {
		t.Fatal("knowledge not shown")
	}
}

func TestLogsErrorGroupsToTask(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			r.ParseForm()
			sent = append(sent, r.Form.Get("search")+" @"+r.Form.Get("earliest_time"))
			w.Write([]byte(`{"sid":"1"}`))
		case strings.HasSuffix(r.URL.Path, "/results"):
			w.Write([]byte(`{"results":[
			 {"service":"bom-worker","sig":"sync MO <id> failed: timeout","count":"7","first":"1762740000","last":"1762743600","sample":"sync MO MO-123 failed: timeout","caller":"usecase/sync.go:42","request_id":"rq-1"},
			 {"service":"bom-api","sig":"record not found","count":"12","first":"1762740000","last":"1762743600","sample":"record not found","caller":"repo/mo.go:10"}]}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Write([]byte(`{"results":[]}`))
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"entry":[{"content":{"isDone":true}}]}`))
		}
	}))
	defer srv.Close()
	m, _ := logsModel(t, srv.URL)
	m, _ = press(t, m, "3", "e")
	m, cmd := press(t, m, "g")
	m = drainLogs(t, m, cmd)
	if len(sent) != 1 || !strings.Contains(sent[0], "by service, sig") || !strings.HasSuffix(sent[0], "@-24h") || strings.Contains(sent[0], "[") {
		t.Fatalf("grouping search = %q", sent)
	}
	if !m.logs.groupsFocus || len(m.logs.groups) != 2 || m.logs.groups[0].Service != "bom-api" {
		t.Fatalf("groups (most frequent first) = %+v", m.logs.groups)
	}
	if v := m.View(); !strings.Contains(v, "2 nhóm · 19 lỗi") || !strings.Contains(v, "record not found") {
		t.Fatalf("groups not shown:\n%s", v)
	}
	// ↓ selects the worker group; Enter drills into its events (caller + phrase).
	m, _ = press(t, m, "down")
	m, cmd = press(t, m, "enter")
	m = drainLogs(t, m, cmd)
	if !strings.Contains(sent[1], `caller="usecase/sync.go:42"`) || !strings.Contains(sent[1], `"failed: timeout"`) || !strings.Contains(sent[1], "bom-worker") {
		t.Fatalf("drill = %q", sent[1])
	}
	// g goes back to the groups (no new search), n opens a prefilled task form.
	m, _ = press(t, m, "g")
	if !m.logs.groupsFocus || m.logs.groupCursor != 1 || len(sent) != 2 {
		t.Fatal("g returns to the groups, keeping the selection, without searching again")
	}
	m, _ = press(t, m, "n")
	if m.modal == nil {
		t.Fatal("n must open the task form")
	}
	f := m.modal
	if f.value("project") != "bom" || f.value("repo") != "bom-worker" || !strings.Contains(f.value("title"), "bom-worker") ||
		!strings.Contains(f.value("body"), "usecase/sync.go:42") || !strings.Contains(f.value("body"), "7 lần") || !strings.Contains(f.value("body"), "kubernetes.namespace_name=bom-prod") {
		t.Fatalf("task prefill: project=%q repo=%q title=%q body=\n%s", f.value("project"), f.value("repo"), f.value("title"), f.value("body"))
	}
}

func TestLogsAskClaude(t *testing.T) {
	m, _ := logsModel(t, "http://127.0.0.1:1")
	var asked splgen.Context
	m.d.SPLGen = func(_ context.Context, lc splgen.Context, q string) (*splgen.Result, error) {
		asked = lc
		return &splgen.Result{SPL: "kubernetes.container_name=bom-worker level=error | stats count by caller", Earliest: "@d", Explanation: "lỗi worker hôm nay theo caller"}, nil
	}
	m, _ = press(t, m, "3", "e", "a")
	if m.modal == nil {
		t.Fatal("a opens the question form")
	}
	m = typeText(m, "worker lỗi gì hôm nay")
	m, cmd := press(t, m, "ctrl+s")
	for _, msg := range runSequence(cmd) {
		tm, _ := m.Update(msg)
		m = tm.(Model)
	}
	if asked.Project != "bom" || !strings.Contains(asked.Knowledge, "bom_request_logs") || len(asked.Services) != 2 {
		t.Fatalf("context given to Claude = %+v", asked)
	}
	if m.modal == nil || m.modal.value("spl") != "kubernetes.container_name=bom-worker level=error | stats count by caller" || m.modal.value("earliest") != "@d" || !strings.Contains(m.modal.hint, "theo caller") {
		t.Fatal("the generated SPL must open for review")
	}
	m.modal.fields[0].area.SetValue("x | delete")
	m, _ = press(t, m, "ctrl+s")
	if m.modal == nil || !strings.Contains(m.modal.err, "delete") {
		t.Fatal("an edited destructive SPL is refused")
	}
}

// runSequence runs a command (tea.Sequence included) and returns its messages.
func runSequence(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if seq, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range seq {
			out = append(out, runSequence(c)...)
		}
		return out
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice {
		var out []tea.Msg
		for i := 0; i < v.Len(); i++ {
			if c, ok := v.Index(i).Interface().(tea.Cmd); ok {
				out = append(out, runSequence(c)...)
			}
		}
		return out
	}
	return []tea.Msg{msg}
}

func drainLogs(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a search command")
	}
	msg := cmd()
	if _, ok := msg.(logsResultMsg); !ok {
		t.Fatalf("unexpected msg %T", msg)
	}
	tm, _ := m.Update(msg)
	return tm.(Model)
}

func TestLogsQCWithArgoCD(t *testing.T) {
	var synced, logQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/applications":
			w.Write([]byte(`{"items":[
			 {"metadata":{"name":"bom-api-qc"},"spec":{"source":{"targetRevision":"release/qc"},"destination":{"namespace":"bom-qc"}},"status":{"health":{"status":"Healthy"},"sync":{"status":"Synced","revision":"abc"},"resources":[{"group":"apps","kind":"Deployment","name":"bom-api-qc","namespace":"bom-qc"}]}},
			 {"metadata":{"name":"bom-worker-qc"},"status":{"health":{"status":"Degraded"},"sync":{"status":"OutOfSync"},"resources":[{"group":"apps","kind":"Deployment","name":"bom-worker-qc","namespace":"bom-qc"}]}},
			 {"metadata":{"name":"hsk-pdp-backend-qc"},"status":{"health":{"status":"Healthy"},"sync":{"status":"Synced"}}}]}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			logQueries = append(logQueries, r.URL.RawQuery)
			w.Write([]byte(`{"result":{"content":"{\"level\":\"error\",\"msg\":\"MO-77 failed\"}","timeStamp":"2026-11-09T10:00:01Z","podName":"p"}}` + "\n" +
				`{"result":{"content":"started","timeStamp":"2026-11-09T10:00:00Z","podName":"p"}}` + "\n"))
		case strings.HasSuffix(r.URL.Path, "/sync"):
			synced = append(synced, r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	m, _, _ := newTest(t)
	m.d.Config.ArgoCD = config.ArgoCD{URL: srv.URL, Env: "qc", MaxLogLines: 100}
	m.d.ArgoCD = func(context.Context) (*argocd.Client, error) {
		return &argocd.Client{BaseURL: srv.URL, Token: "t"}, nil
	}

	m, cmd := press(t, m, "3")
	m = drainAny(t, m, cmd)
	if m.logs.env != envQC || len(m.qcApps()) != 2 || !strings.Contains(m.View(), "bom-worker-qc") || strings.Contains(m.View(), "hsk-pdp-backend-qc") {
		t.Fatalf("QC view for bom (plm app must not show):\n%s", m.View())
	}
	// ← / → switch project: plm shows its own app.
	m, _ = press(t, m, "right")
	if apps := m.qcApps(); len(apps) != 1 || apps[0].Name != "hsk-pdp-backend-qc" {
		t.Fatalf("plm apps = %v", apps)
	}
	m, _ = press(t, m, "left")
	// Enter loads logs of the selected app's Deployment.
	m, cmd = press(t, m, "enter")
	m = drainAny(t, m, cmd)
	if !strings.Contains(m.View(), "MO-77 failed") || !strings.Contains(logQueries[0], "resourceName=bom-api-qc") {
		t.Fatalf("logs:\n%s\n%v", m.View(), logQueries)
	}
	// Mouse: click the second app row (y=5: header, border, env line, project line, row0=4).
	m, _ = click(m, 3, 5)
	if c := m.qcCurrent(); c == nil || c.Name != "bom-worker-qc" {
		t.Fatalf("click selected %v", c)
	}
	// S asks first; n cancels, y syncs.
	m, cmd = press(t, m, "S")
	if m.conf == nil || cmd != nil || len(synced) != 0 {
		t.Fatal("sync must ask for confirmation")
	}
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "S")
	var tm tea.Model
	tm, cmd = m.Update(key("y"))
	m = drainAny(t, tm.(Model), cmd)
	if len(synced) != 1 || !strings.Contains(synced[0], "bom-worker-qc") {
		t.Fatalf("synced = %v", synced)
	}
	// e switches to Production and back without losing the QC pane.
	m, _ = press(t, m, "e")
	if m.logs.env != envProd {
		t.Fatal("e should switch to production")
	}
	m, _ = press(t, m, "e")
	if m.logs.env != envQC || !strings.Contains(m.View(), "bom-worker-qc") {
		t.Fatalf("back to QC:\n%s", m.View())
	}
}

// drainAny runs async commands (Argo CD / Splunk / Hasaki) and feeds their messages back.
func drainAny(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil && i < 10; i++ {
		msg := cmd()
		switch msg.(type) {
		case qcAppsMsg, qcLogsMsg, qcSyncMsg, logsResultMsg, hwTasksMsg, hwDetailMsg:
		default:
			return m
		}
		var tm tea.Model
		tm, cmd = m.Update(msg)
		m = tm.(Model)
	}
	return m
}

type fakeHerdr struct{ split, ran []string }

func (f *fakeHerdr) SplitRight(_ context.Context, cwd string) (string, error) {
	f.split = append(f.split, cwd)
	return "w1:p7", nil
}
func (f *fakeHerdr) Run(_ context.Context, pane, command string) error {
	f.ran = append(f.ran, pane+" "+command)
	return nil
}

func TestTakeOverOpensHerdrPane(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "hw-HSK-1", Title: "x", Workspace: "/ws/hw-HSK-1", Runs: 1, SessionID: "s"}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)
	h := &fakeHerdr{}
	m.d.Herdr, m.d.Exe = h, "/bin/taskpilot"
	m.reloadTasks()
	m, cmd := press(t, m, "o")
	if cmd == nil {
		t.Fatal("expected a command")
	}
	tm, _ := m.Update(cmd())
	m = tm.(Model)
	if len(h.split) != 1 || h.split[0] != "/ws/hw-HSK-1" || len(h.ran) != 1 ||
		h.ran[0] != "w1:p7 /bin/taskpilot takeover hw-HSK-1 -config /tmp/tp.yaml" {
		t.Fatalf("split=%v ran=%v", h.split, h.ran)
	}
	if !strings.Contains(m.msg, "w1:p7") {
		t.Fatalf("msg = %q", m.msg)
	}
	if st.TakenOver(tk.ID) {
		t.Fatal("the TUI must not mark the task; the takeover command in the pane does")
	}
}

// runSeq runs the commands of a tea.Sequence in order and feeds their messages back.
func runSeq(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	msg := cmd()
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		t.Fatalf("want a sequence, got %T", msg)
	}
	var tm tea.Model = m
	for i := range v.Len() {
		if c, ok := v.Index(i).Interface().(tea.Cmd); ok && c != nil {
			tm, _ = tm.Update(c())
		}
	}
	return tm.(Model)
}

func TestTaskFromPrompt(t *testing.T) {
	m, _, inbox := newTest(t)
	var got string
	m.d.Draft = func(_ context.Context, prompt string) (*intake.Draft, error) {
		got = prompt
		return &intake.Draft{Project: "bom", Repo: "bom-worker", Title: "Sửa export MO", Body: "Export MO timeout.", Note: "đoán repo từ chữ worker"}, nil
	}
	m, _ = press(t, m, "N")
	if m.modal == nil || m.modal.title != "Task từ prompt" {
		t.Fatal("N should open the prompt form")
	}
	m = typeText(m, "worker export MO bị timeout")
	m, cmd := press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("prompt form error: %s", m.modal.err)
	}
	m = runSeq(t, m, cmd)
	if got != "worker export MO bị timeout" {
		t.Fatalf("draft called with %q", got)
	}
	if m.tasks.drafting || m.modal == nil || m.modal.title != "Task mới" || !strings.Contains(m.modal.hint, "đoán repo từ chữ worker") {
		t.Fatalf("draft should open the prefilled task form: %+v", m.modal)
	}
	if v := m.modal.values(); v["project"] != "bom" || v["repo"] != "bom-worker" || v["title"] != "Sửa export MO" {
		t.Fatalf("form values = %v", v)
	}
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form error: %s", m.modal.err)
	}
	tk := readTicket(t, inbox)
	if tk.Project != "bom" || tk.Repo != "bom-worker" || tk.Title != "Sửa export MO" ||
		!strings.Contains(tk.Body, "Export MO timeout.") || !strings.Contains(tk.Body, "worker export MO bị timeout") {
		t.Fatalf("ticket = %+v", tk)
	}
}

func TestTaskFromPromptError(t *testing.T) {
	m, _, _ := newTest(t)
	m.d.Draft = func(context.Context, string) (*intake.Draft, error) { return nil, errors.New("claude: not logged in") }
	m, _ = press(t, m, "N")
	m = typeText(m, "làm gì đó")
	m, cmd := press(t, m, "ctrl+s")
	m = runSeq(t, m, cmd)
	if m.modal == nil || m.modal.title != "Task từ prompt" || m.modal.value("prompt") != "làm gì đó" || !strings.Contains(m.modal.err, "not logged in") {
		t.Fatalf("error should reopen the prompt with the text kept: %+v", m.modal)
	}
}

func TestTaskFromPromptDraftWhileModalOpen(t *testing.T) {
	m, _, _ := newTest(t)
	m.d.Draft = func(context.Context, string) (*intake.Draft, error) { return nil, nil }
	m, _ = press(t, m, "n") // operator is busy in another form
	var tm tea.Model
	tm, _ = m.Update(draftMsg{prompt: "p", draft: &intake.Draft{Project: "plm", Title: "T", Body: "B"}})
	m = tm.(Model)
	if m.modal.title != "Task mới" || m.modal.value("title") != "" || m.tasks.draft == nil {
		t.Fatal("an arriving draft must not replace the open form")
	}
	m, _ = press(t, m, "esc", "N")
	if m.modal == nil || m.modal.value("title") != "T" || m.modal.value("project") != "plm" || m.tasks.draft != nil {
		t.Fatalf("N should show the parked draft: %+v", m.modal)
	}
}

func TestTaskFromPromptUnavailable(t *testing.T) {
	m, _, _ := newTest(t)
	m, _ = press(t, m, "N")
	if m.modal != nil || !strings.Contains(m.msg, "bấm n") {
		t.Fatalf("without Draft, N should explain: modal=%v msg=%q", m.modal, m.msg)
	}
}

func TestHWListHidesEpics(t *testing.T) {
	m, _, _ := newTest(t)
	m.d.HW = fakeHW{tasks: []hasakiwork.Task{
		{ID: 1, Code: "HSK-AAA", Name: "Sửa logic MO", ProjectID: 8388},
		{ID: 2, Code: "HSK-E1", Name: "[EPIC] DEV Sprint 10", ProjectID: 8388},
		{ID: 3, Code: "HSK-E2", Name: "[EPIC] Improve Sprint 10", ProjectID: 8388},
		{ID: 4, Code: "HSK-E3", Name: "[EPIC] Issue Production", ProjectID: 8388},
	}}
	m, cmd := press(t, m, "2")
	m = drain(t, m, cmd)
	if len(m.hw.list) != 1 || m.hw.list[0].Code != "HSK-AAA" {
		t.Fatalf("only the task to do must be listed: %+v", m.hw.list)
	}
	if v := m.View(); !strings.Contains(v, "ẩn 3 EPIC") || strings.Contains(v, "[EPIC]") {
		t.Fatalf("EPICs must be hidden and counted:\n%s", v)
	}
}

func TestF2TogglesMouse(t *testing.T) {
	m, _, _ := newTest(t)
	m, cmd := press(t, m, "f2")
	if !m.mouseOff || cmd == nil || !strings.Contains(m.View(), "chuột: bôi đen") {
		t.Fatal("F2 hands the mouse to the terminal")
	}
	if msg := cmd(); fmt.Sprintf("%T", msg) != fmt.Sprintf("%T", tea.DisableMouse()) {
		t.Fatalf("cmd = %T", msg)
	}
	tm, cmd := m.Update(execDoneMsg{})
	if cmd == nil || fmt.Sprintf("%T", cmd()) != fmt.Sprintf("%T", tea.DisableMouse()) {
		t.Fatal("after an external command the mouse stays off")
	}
	m = tm.(Model)
	m, cmd = press(t, m, "f2")
	if m.mouseOff || cmd == nil || fmt.Sprintf("%T", cmd()) != fmt.Sprintf("%T", tea.EnableMouseCellMotion()) {
		t.Fatal("F2 again gives the mouse back to the TUI")
	}
}

func TestPinProfile(t *testing.T) {
	m, st, _ := newTest(t)
	m.d.Config.Claude.Models = []string{"fable", "opus", "sonnet"}
	tk := &store.Task{ID: "mock-e", Title: "x", Project: "bom"}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)
	st.SetTaskProfile("mock-e", &store.Profile{Model: "opus", Effort: "high", Reason: "cross-repo, money", SetBy: "agent"})
	m.reloadTasks()
	if v := m.View(); !strings.Contains(v, "model opus · effort high (agent chọn") || !strings.Contains(v, "cross-repo, money") {
		t.Fatalf("detail must show the agent's choice:\n%s", v)
	}
	m, _ = press(t, m, "e")
	if m.modal == nil || !strings.Contains(m.modal.hint, "Agent đang chọn: opus / high") {
		t.Fatal("e opens the profile form")
	}
	m, _ = press(t, m, "right", "right", "right") // model: agent → fable → opus → sonnet
	m, _ = press(t, m, "tab", "right")            // effort: agent → low
	m, _ = press(t, m, "ctrl+s")
	if p, _ := st.TaskProfile("mock-e"); p == nil || p.SetBy != "operator" || p.Model != "sonnet" || p.Effort != "low" {
		t.Fatalf("pinned = %+v", p)
	}
	// Both back to "agent tự chọn": the pin is removed.
	m, _ = press(t, m, "e")
	m, _ = press(t, m, "left", "left", "left", "tab", "left", "ctrl+s")
	if p, _ := st.TaskProfile("mock-e"); p != nil {
		t.Fatalf("unpin must clear the profile, got %+v", p)
	}
}
