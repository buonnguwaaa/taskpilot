package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) refreshStatus() {
	w := max(20, m.stVP.Width)
	var b strings.Builder
	b.WriteString(titleStyle.Render("Đăng nhập") + "\n")
	if m.d.AuthStatus != nil {
		for _, l := range m.d.AuthStatus() {
			b.WriteString("  " + l + "\n")
		}
	}
	b.WriteString(dimStyle.Render("  l: đăng nhập Hasaki Work · s: đăng nhập Splunk · S: đăng nhập + tạo token Splunk 90 ngày · a: đăng nhập Argo CD QC · t: dán JWT · b: lưu token Bitbucket") + "\n\n")

	b.WriteString(titleStyle.Render("Thống kê") + "\n")
	if st, err := m.d.Store.Stats(); err != nil {
		b.WriteString(errStyle.Render("  "+err.Error()) + "\n")
	} else {
		fmt.Fprintf(&b, "  chi phí hôm nay $%.2f · 7 ngày $%.2f (%d lượt chạy) · %d task xong trong 7 ngày\n", st.CostToday, st.Cost7d, st.Runs7d, st.TasksDone7d)
		for _, g := range st.Gates {
			rate := "-"
			if g.ApprovalRate >= 0 {
				rate = fmt.Sprintf("%.0f%%", g.ApprovalRate*100)
			}
			fmt.Fprintf(&b, "  cổng %-6s duyệt %s (%d/%d) · đang chờ %d · bạn phản hồi TB sau %.0f phút\n",
				g.Gate, rate, g.Approved, g.Approved+g.Rejected, g.Pending, g.AvgWaitMinutes)
		}
		if len(st.TopDenials) > 0 {
			fmt.Fprintf(&b, "  agent bị chặn nhiều nhất: %s\n", strings.Join(st.TopDenials, ", "))
		}
		if st.OutboxPending+st.OutboxFailed > 0 {
			fmt.Fprintf(&b, "  báo cáo về ticket: %d đang chờ gửi lại, %d thất bại\n", st.OutboxPending, st.OutboxFailed)
		}
		b.WriteString(dimStyle.Render("  chi tiết: `:db stats`, `:db runs <task>`, `:db sql \"SELECT …\"`") + "\n")
	}
	b.WriteString("\n")

	b.WriteString(titleStyle.Render("Daemon") + "\n")
	switch d := m.d.Daemon; {
	case d.Embedded:
		n := 0
		if d.InFlight != nil {
			n = d.InFlight()
		}
		fmt.Fprintf(&b, "  chạy trong TUI này · %d task đang chạy · thoát TUI sẽ dừng daemon\n", n)
	case d.External:
		b.WriteString("  một tiến trình khác (`taskpilot daemon` hoặc TUI khác) đang lập lịch task\n")
	default:
		b.WriteString(warnStyle.Render("  không chạy — task sẽ không được xử lý") + "\n")
	}
	b.WriteString("\n" + titleStyle.Render("Cấu hình") + "\n")
	fmt.Fprintf(&b, "  %s\n", m.d.Config.Path)
	for _, name := range m.d.Config.ProjectNames() {
		p := m.d.Config.Projects[name]
		fmt.Fprintf(&b, "  %-8s %-22s %2d repo   %s\n", name, p.Name, len(p.Repos), dimStyle.Render(strings.Join(p.RepoNames(), ", ")))
	}
	b.WriteString(dimStyle.Render("  d: chạy doctor (kiểm tra repo, nhánh, công cụ, đăng nhập)") + "\n")

	if m.d.Daemon.Logs != nil {
		b.WriteString("\n" + titleStyle.Render("Log daemon") + "\n")
		lines := m.d.Daemon.Logs()
		if len(lines) > 200 {
			lines = lines[len(lines)-200:]
		}
		if len(lines) == 0 {
			b.WriteString(dimStyle.Render("  (chưa có)") + "\n")
		}
		for _, l := range lines {
			b.WriteString("  " + truncate(l, w-2) + "\n")
		}
	}
	atBottom := m.stVP.AtBottom()
	m.stVP.SetContent(b.String())
	if atBottom {
		m.stVP.GotoBottom()
	}
}

func (m Model) statusKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "l":
		return m.runCommand("auth login")
	case "t":
		return m.runCommand("auth set-token")
	case "b":
		return m.runCommand("auth bitbucket")
	case "s":
		return m.runCommand("auth splunk")
	case "a":
		return m.runCommand("auth argocd")
	case "S":
		return m.runCommand("auth splunk -create")
	case "d":
		return m.runCommand("doctor")
	case "r":
		m.refreshBanner()
		m.refreshStatus()
		m.msg = "đã làm mới"
	case "j", "down":
		m.stVP.ScrollDown(1)
	case "k", "up":
		m.stVP.ScrollUp(1)
	case "pgdown", " ":
		m.stVP.HalfViewDown()
	case "pgup":
		m.stVP.HalfViewUp()
	}
	return m, nil
}

// execTaskpilot builds a command for the taskpilot binary bound to this config.
func (m Model) execTaskpilot(args ...string) *exec.Cmd {
	cmd := exec.Command(m.d.Exe, args...)
	cmd.Env = append(os.Environ(), "TASKPILOT_CONFIG="+m.d.Config.Path)
	return cmd
}
