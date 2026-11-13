package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestSubtaskNamesAndDeadline(t *testing.T) {
	got := subtaskNames("- Viết API\n\n2. Viết test \n* Cập nhật docs\n   ")
	if strings.Join(got, "|") != "Viết API|Viết test|Cập nhật docs" {
		t.Fatalf("names = %q", got)
	}
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.Local)
	if d := subtaskDeadline(hasakiwork.Task{DateEnd: "2026-11-30 00:00:00"}, now); d != "2026-11-30" {
		t.Errorf("future parent deadline: %s", d)
	}
	if d := subtaskDeadline(hasakiwork.Task{DateEnd: "2026-11-01 00:00:00"}, now); d != "2026-11-10" {
		t.Errorf("past parent deadline → today: %s", d)
	}
}

func hwParentModel(t *testing.T) (Model, *store.Store, *[]hasakiwork.NewTask, *[][2]int64) {
	t.Helper()
	m, st, _ := newTest(t)
	var created []hasakiwork.NewTask
	var assigned [][2]int64
	parent := hasakiwork.Task{ID: 14556925, Code: "HSK-PARENT", Name: "Tích hợp VNPAY trả góp", ProjectID: 7771,
		MilestoneID: 10297539, DateEnd: "2099-12-31 00:00:00", Project: &hasakiwork.Ref{ID: 7771, Name: "Payment"}}
	m.d.HW = fakeHW{tasks: []hasakiwork.Task{parent}, created: &created, assigned: &assigned}
	m.d.Config.Projects["payment"] = m.d.Config.Projects["bom"]
	return m, st, &created, &assigned
}

func TestCreateSubtasksFromHasakiTab(t *testing.T) {
	m, _, created, assigned := hwParentModel(t)
	m, cmd := press(t, m, "2")
	m = drain(t, m, cmd)
	m, _ = press(t, m, "n")
	if m.modal == nil || !strings.Contains(m.modal.title, "Subtask của HSK-PARENT") || m.modal.value("deadline") != "2099-12-31" {
		t.Fatalf("subtask form: %+v", m.modal)
	}
	m = typeText(m, "Viết API trả góp")
	m, _ = press(t, m, "enter")
	m = typeText(m, "Viết test")
	m, cmd = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("submit: %s", m.modal.err)
	}
	var tm tea.Model = m
	tm, _ = tm.Update(cmd())
	m = tm.(Model)
	if len(*created) != 2 {
		t.Fatalf("created %d", len(*created))
	}
	for i, want := range []string{"Viết API trả góp", "Viết test"} {
		c := (*created)[i]
		if c.Name != want || c.ParentID != 14556925 || c.MilestoneID != 10297539 || c.ProjectID != 7771 || c.PlannedMinutes != 240 {
			t.Errorf("subtask %d = %+v", i, c)
		}
	}
	if len(*assigned) != 2 || (*assigned)[0][1] != 39324 {
		t.Fatalf("assigned = %v", *assigned)
	}
	if !strings.Contains(m.msg, "đã tạo 2 subtask dưới HSK-PARENT: HSK-NEW1, HSK-NEW2") {
		t.Fatalf("msg = %q", m.msg)
	}
}

// In the Tasks tab, h on a task that came from Hasaki Work adds subtasks to it.
func TestSubtaskFromDispatchedTask(t *testing.T) {
	m, st, _, _ := hwParentModel(t)
	m, cmd := press(t, m, "2")
	m = drain(t, m, cmd)
	tk := &store.Task{ID: "hw-HSK-PARENT", Source: hwSource, TicketID: "HSK-PARENT", Title: "x", Project: "payment"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	m, _ = press(t, m, "1")
	m.reloadTasks()
	m, _ = press(t, m, "H")
	if m.modal == nil || !strings.Contains(m.modal.title, "Subtask của HSK-PARENT") {
		t.Fatalf("H on a Hasaki Work task opens the subtask form: modal=%v msg=%q", m.modal != nil, m.msg)
	}
}
