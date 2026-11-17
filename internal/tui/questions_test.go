package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func questionTask(t *testing.T, gate string, qs []store.Question) (Model, *store.Store, *store.ApprovalRequest) {
	t.Helper()
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-ask", Title: "VNPAY trả góp", Project: "payment"}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)
	r, err := st.AddRequestWithQuestions("mock-ask", gate, "Cần bạn chọn", "chi tiết plan", qs)
	if err != nil {
		t.Fatal(err)
	}
	m.reloadTasks()
	return m, st, r
}

func decision(t *testing.T, st *store.Store, r *store.ApprovalRequest) *store.Decision {
	t.Helper()
	decs, _ := st.Decisions("mock-ask")
	return decs[r.ID]
}

var storageQ = store.Question{Question: "Lưu thông tin trả góp ở đâu?", Options: []store.Option{
	{Label: "Bảng mới", Description: "(khuyến nghị) tách bạch"}, {Label: "Cột JSON"}, {Label: "Không lưu"}}}

func TestQuickPickAnswersWithADigit(t *testing.T) {
	m, st, r := questionTask(t, store.QuestionGate, []store.Question{storageQ})
	v := m.View()
	for _, want := range []string{"Agent hỏi bạn", "1-3 chọn · 0 tự gõ", "1) Bảng mới", "(khuyến nghị) tách bạch", "3) Không lưu", "0) Khác"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in:\n%s", want, v)
		}
	}
	m, _ = press(t, m, "2")
	if m.tab != tabTasks {
		t.Fatal("the digit picks an option instead of switching tabs")
	}
	d := decision(t, st, r)
	if d == nil || !d.Approved || !strings.Contains(d.Comment, "→ Cột JSON") || !strings.Contains(d.Comment, storageQ.Question) {
		t.Fatalf("decision = %+v", d)
	}
	// With nothing to answer, digits switch tabs again.
	m, _ = press(t, m, "2")
	if m.tab != tabHasaki {
		t.Fatal("digits switch tabs when no question is waiting")
	}
}

func TestAnswerFormWithMultiAndOther(t *testing.T) {
	multi := store.Question{Question: "Hỗ trợ ngân hàng nào?", Multi: true, Options: []store.Option{{Label: "VCB"}, {Label: "TCB"}, {Label: "ACB"}}}
	m, st, r := questionTask(t, store.QuestionGate, []store.Question{storageQ, multi})
	m, _ = press(t, m, "a")
	if m.modal == nil {
		t.Fatal("a opens the answer form")
	}
	m, _ = press(t, m, "right", "right", "right") // q0: Bảng mới → Cột JSON → Không lưu → Khác…
	m, _ = press(t, m, "tab")                     // o0
	m = typeText(m, "bảng installments có index theo order")
	m, _ = press(t, m, "tab")                      // q1 (multi)
	m, _ = press(t, m, " ", "right", "right", " ") // tick VCB and ACB
	m, cmd := press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form error: %s", m.modal.err)
	}
	_ = cmd
	d := decision(t, st, r)
	if d == nil || !d.Approved {
		t.Fatalf("decision = %+v", d)
	}
	for _, want := range []string{"→ Khác: bảng installments có index theo order", "Q2: Hỗ trợ ngân hàng nào?\n→ VCB; ACB"} {
		if !strings.Contains(d.Comment, want) {
			t.Errorf("answer missing %q:\n%s", want, d.Comment)
		}
	}
}

func TestOtherWithoutTextIsRefused(t *testing.T) {
	m, _, _ := questionTask(t, store.QuestionGate, []store.Question{storageQ})
	m, _ = press(t, m, "a", "left") // q0: Bảng mới ← Khác…
	m, _ = press(t, m, "ctrl+s")
	if m.modal == nil || !strings.Contains(m.modal.err, "Khác") {
		t.Fatal("Khác without an answer must be refused")
	}
}

// A plan gate with questions: approve and answer in one form; m gives
// feedback without approving.
func TestGateWithQuestions(t *testing.T) {
	m, st, r := questionTask(t, "plan", []store.Question{storageQ})
	if v := m.View(); !strings.Contains(v, "Enter duyệt + gửi · x từ chối + gửi") || !strings.Contains(v, "Câu hỏi kèm theo") {
		t.Fatalf("gate view:\n%s", v)
	}
	m, _ = press(t, m, "1") // digits do not decide a gate
	if decision(t, st, r) != nil {
		t.Fatal("a gate is never decided by a digit")
	}
	m, _ = press(t, m, "1") // back to the tasks tab
	m, _ = press(t, m, "a")
	if m.modal == nil || m.modal.value("decision") != "approve" {
		t.Fatal("a opens the answer form with Duyệt preselected")
	}
	m, _ = press(t, m, "tab", "right") // q0 → Cột JSON
	var tm tea.Model = m
	tm, _ = tm.Update(key("ctrl+s"))
	if d := decision(t, st, r); d == nil || !d.Approved || !strings.Contains(d.Comment, "→ Cột JSON") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestReplyWithMWhileWaiting(t *testing.T) {
	m, st, r := questionTask(t, "pr", nil)
	m, _ = press(t, m, "m")
	if m.modal == nil || !strings.Contains(m.modal.title, "Góp ý cho pr-1") {
		t.Fatal("m on a gate opens a feedback form")
	}
	m = typeText(m, "đổi tên biến cho rõ")
	m, _ = press(t, m, "ctrl+s")
	if d := decision(t, st, r); d == nil || d.Approved || d.Comment != "đổi tên biến cho rõ" {
		t.Fatalf("feedback = %+v", d)
	}
}

// The case of the screenshot: a plan gate with two questions, answered in
// the detail pane with digits, then Enter approves and sends the answers.
func TestInlineAnswersOnAGate(t *testing.T) {
	q2 := store.Question{Question: "Chặn luôn ở khâu tạo MO?", Options: []store.Option{{Label: "Không, chỉ sửa gen WO"}, {Label: "Có, sửa cả tạo MO"}}}
	m, st, r := questionTask(t, "plan", []store.Question{storageQ, q2})
	if !strings.Contains(m.tasks.lastBody, "1-3 chọn · 0 tự gõ · ←/→ đổi câu · Enter duyệt + gửi") {
		t.Fatalf("keys not shown:\n%s", m.tasks.lastBody)
	}
	// The questions are shown before the plan details.
	if body := ansi.Strip(m.tasks.lastBody); strings.Index(body, "Câu hỏi kèm theo") > strings.Index(body, "chi tiết plan") {
		t.Fatal("questions must come before the details")
	}
	m, _ = press(t, m, "enter")
	if decision(t, st, r) != nil || !strings.Contains(m.msg, "còn câu chưa trả lời") {
		t.Fatal("Enter waits for every answer")
	}
	m, _ = press(t, m, "2") // Q1 → Cột JSON, moves to Q2
	if a := m.tasks.answer; a == nil || !a.picks[0][1] || a.cur != 1 {
		t.Fatalf("answer state = %+v", m.tasks.answer)
	}
	if !strings.Contains(m.tasks.lastBody, "✓ 2) Cột JSON") {
		t.Fatalf("the choice is marked:\n%s", m.tasks.lastBody)
	}
	m, _ = press(t, m, "0") // Q2: another answer
	if m.modal == nil {
		t.Fatal("0 opens the free answer")
	}
	m = typeText(m, "chặn ở cả hai, nhưng tách PR")
	m, cmd := press(t, m, "ctrl+s")
	m = run1(t, m, cmd)
	if m.tab != tabTasks {
		t.Fatal("digits never switch tabs while questions wait")
	}
	m, _ = press(t, m, "enter")
	d := decision(t, st, r)
	if d == nil || !d.Approved || !strings.Contains(d.Comment, "→ Cột JSON") || !strings.Contains(d.Comment, "→ Khác: chặn ở cả hai, nhưng tách PR") {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(m.msg, "đã duyệt plan-1 kèm câu trả lời") {
		t.Fatalf("msg = %q", m.msg)
	}
}

func TestInlineRejectWithAnswersAndMulti(t *testing.T) {
	multi := store.Question{Question: "Ngân hàng?", Multi: true, Options: []store.Option{{Label: "VCB"}, {Label: "TCB"}, {Label: "ACB"}}}
	m, st, r := questionTask(t, "pr", []store.Question{multi})
	m, _ = press(t, m, "1", "3") // tick VCB and ACB
	m, _ = press(t, m, "x")
	d := decision(t, st, r)
	if d == nil || d.Approved || !strings.Contains(d.Comment, "→ VCB; ACB") {
		t.Fatalf("decision = %+v", d)
	}
}
