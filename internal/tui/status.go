package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/graphify"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// The Status tab is a set of cards: connections, daemon, activity, projects
// and the daemon log, each with its own colored frame; two columns when the
// screen is wide.

var (
	okMark   = lipgloss.NewStyle().Foreground(clr(cOk)).Render("✓")
	warnMark = lipgloss.NewStyle().Foreground(clr(cAccent)).Render("!")
	errMark  = lipgloss.NewStyle().Foreground(clr(cErr)).Render("✗")
	offMark  = dimStyle.Render("○")
	keyStyle = lipgloss.NewStyle().Foreground(clr(cBright)).Background(clr(cSurface))
)

// card frames body with a colored border and a title.
func card(title, color, body string, width int) string {
	c := clr(color)
	head := fg(cBright).Render(title)
	if color == cErr {
		head = errStyle.Render(title)
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c).Padding(0, 1).
		Width(max(10, width-2)).Render(head + "\n" + strings.TrimRight(body, "\n"))
}

// keyCap renders a key, e.g. [ s ].
func keyCap(k string) string { return keyStyle.Render(" " + k + " ") }

// connMark grades a connection status line.
func connMark(text string) string {
	l := strings.ToLower(text)
	switch {
	case strings.Contains(l, "chưa cấu hình"):
		return offMark
	case strings.Contains(l, "tự gia hạn") && strings.Contains(l, "đã hết hạn") && !strings.Contains(l, "phiên đăng nhập đã hết hạn"):
		return warnMark // the token renews itself from the session cookie
	case strings.Contains(l, "đã hết hạn"), strings.Contains(l, "chưa đăng nhập"), strings.Contains(l, "chưa có token —"):
		return errMark
	case strings.Contains(l, "sắp hết hạn"), strings.Contains(l, "không có"), strings.Contains(l, "chưa có token"):
		return warnMark
	}
	return okMark
}

// connKeys maps a connection to the key that logs in to it.
var connKeys = []struct{ prefix, key string }{
	{"Hasaki Work", "l"}, {"Splunk", "s"}, {"Argo CD", "a"}, {"Vault", "v"}, {"Kafka UI", "K"}, {"Bitbucket", "b"},
}

func (m Model) connectionsCard(w int) string {
	var b strings.Builder
	var lines []string
	if m.d.AuthStatus != nil {
		lines = m.d.AuthStatus()
	}
	nameW := 0
	type conn struct{ name, text, key string }
	var conns []conn
	for _, l := range lines {
		if strings.HasPrefix(l, "  ") && len(conns) > 0 { // a detail of the previous one
			conns[len(conns)-1].text += "\n" + strings.TrimSpace(l)
			continue
		}
		name, text, ok := strings.Cut(l, ": ")
		if !ok {
			name, text = "", l
		}
		c := conn{name: name, text: text}
		for _, k := range connKeys {
			if strings.HasPrefix(name, k.prefix) {
				c.key = k.key
			}
		}
		nameW = max(nameW, lipgloss.Width(name))
		conns = append(conns, c)
	}
	textW := max(10, w-4-nameW-10)
	for _, c := range conns {
		parts := strings.Split(c.text, "\n")
		key := "     "
		if c.key != "" {
			key = keyCap(c.key) + "  "
		}
		fmt.Fprintf(&b, "%s %s %-*s  %s\n", key, connMark(parts[0]), nameW, titleStyle.Render(padRight(c.name, nameW)), truncate(parts[0], textW))
		for _, d := range parts[1:] {
			fmt.Fprintf(&b, "%s   %s  %s\n", strings.Repeat(" ", 5), strings.Repeat(" ", nameW), dimStyle.Render(truncate(d, textW)))
		}
	}
	return card("Kết nối", cRule, b.String(), w)
}

func padRight(s string, n int) string {
	if d := n - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func (m Model) daemonCard(w int) string {
	var b strings.Builder
	color := cRule
	switch d := m.d.Daemon; {
	case d.Embedded:
		n := 0
		if d.InFlight != nil {
			n = d.InFlight()
		}
		fmt.Fprintf(&b, "%s chạy trong TUI này · %s task đang chạy\n", okMark, titleStyle.Render(fmt.Sprint(n)))
		b.WriteString(dimStyle.Render("thoát TUI sẽ dừng daemon") + "\n")
	case d.External:
		b.WriteString(okMark + " đang chạy ở tiến trình khác (`taskpilot daemon` / tab herdr)\n")
	default:
		color = cErr
		b.WriteString(errMark + " " + errStyle.Render("không chạy: task sẽ không được xử lý") + "\n")
	}
	if names := m.scanProjects(); len(names) > 0 {
		b.WriteString("\n" + titleStyle.Render("Quét lỗi production") + "\n")
		for _, name := range names {
			p := m.d.Config.Projects[name]
			line := titleStyle.Render(padRight(name, 10)) + " " + truncate("lịch "+strings.Join(p.LogsProduction.Scan.Times, ", "), w-30)
			if n := m.suggestionCount(name); n > 0 {
				line += warnStyle.Render(fmt.Sprintf(" · %d đề xuất", n))
			}
			mark, detail := offMark, "chưa quét lần nào"
			if last, _ := m.d.Store.LastScan(name); last != nil {
				mark, detail = okMark, "lần cuối "+last.StartedAt.Local().Format("02/01 15:04")
				if last.Error != "" {
					mark, detail = errMark, detail+" · lỗi: "+last.Error
				}
			}
			b.WriteString(mark + " " + line + "\n")
			b.WriteString("  " + strings.Repeat(" ", 11) + dimStyle.Render(truncate(detail, w-17)) + "\n")
		}
	}
	return card("Daemon", color, b.String(), w)
}

// scanProjects are the projects with a scheduled error scan.
func (m Model) scanProjects() []string {
	var out []string
	for _, name := range m.d.Config.ProjectNames() {
		if p := m.d.Config.Projects[name]; p.LogsProduction != nil && p.LogsProduction.Scan != nil {
			out = append(out, name)
		}
	}
	return out
}

// graphCard shows the graphify graphs (what each line was built from) and
// whether they pay off: how agents used graphify, and their planning runs
// next to those of tasks that did not use it.
func (m Model) graphCard(w int) string {
	g := m.d.Config.Graphify
	if g == nil {
		return card("Graphify", cRule, dimStyle.Render(wrap("Chưa bật. Thêm `graphify: {}` vào config để agent hiểu code xuyên repo (taskpilot graphify build).", w-4)), w)
	}
	var b strings.Builder
	for _, name := range m.d.Config.ProjectNames() {
		for _, line := range m.d.Config.ProjectGraphLines(name) {
			st := graphify.ReadState(g.Dir, name, line)
			label := padRight(name+"/"+line, 14)
			switch {
			case st.MergedAt.IsZero():
				b.WriteString(offMark + " " + label + dimStyle.Render(" chưa dựng (:graphify build)") + "\n")
				continue
			case st.Error != "":
				b.WriteString(errMark + " " + label + errStyle.Render(" "+truncate(st.Error, w-30)) + "\n")
				continue
			}
			ok := 0
			for _, rs := range st.Repos {
				if rs.Error == "" {
					ok++
				}
			}
			b.WriteString(okMark + " " + label + dimStyle.Render(fmt.Sprintf(" %d repo · %d node · %d liên kết gRPC · %s",
				ok, st.Nodes, st.Links, st.MergedAt.Local().Format("02/01 15:04"))) + "\n")
		}
	}
	u, err := m.d.Store.GraphUsage(time.Now().AddDate(0, 0, -30))
	if err != nil {
		b.WriteString(errStyle.Render(err.Error()))
		return card("Graphify", cErr, b.String(), w)
	}
	b.WriteString("\n" + titleStyle.Render("Agent dùng graph (30 ngày)") + " " +
		dimStyle.Render("chỉ task chạy sau khi project có graph") + "\n")
	if u.Calls == 0 {
		b.WriteString(dimStyle.Render(wrap(fmt.Sprintf("Chưa có lần gọi graphify nào trong %d task gần đây. Agent được nhắc dùng graphify ở prompt đầu và khi xin duyệt plan.", u.Tasks), w-4)) + "\n")
	} else {
		var acts []string
		for _, a := range []string{"links", "affected", "path", "explain", "query", "read", "status"} {
			if n := u.ByAction[a]; n > 0 {
				acts = append(acts, fmt.Sprintf("%s %d", a, n))
			}
		}
		big := fg(cBright)
		fmt.Fprintf(&b, "%s lần gọi · %s/%d task có hỏi graph", big.Render(fmt.Sprint(u.Calls)), big.Render(fmt.Sprint(u.GraphTasks)), u.Tasks)
		if u.Errors > 0 {
			b.WriteString(warnStyle.Render(fmt.Sprintf(" · %d lỗi", u.Errors)))
		}
		b.WriteString("\n" + dimStyle.Render(strings.Join(acts, " · ")) + "\n")
	}
	if u.SearchRuns > 0 {
		pct := u.GraphRuns * 100 / u.SearchRuns
		style := lipgloss.NewStyle().Foreground(clr(cOk))
		if pct < 50 {
			style = warnStyle
		}
		b.WriteString(style.Render(fmt.Sprintf("%d/%d lượt chạy có grep code đã hỏi graph (%d%%)", u.GraphRuns, u.SearchRuns, pct)) + "\n")
	}
	const minTasks = 3 // fewer per group and the averages are one task's story
	if u.With.Tasks+u.Without.Tasks > 0 && (u.With.Tasks < minTasks || u.Without.Tasks < minTasks) {
		b.WriteString("\n" + titleStyle.Render("Lượt lập plan: có graph vs không") + "\n")
		b.WriteString(dimStyle.Render(wrap(fmt.Sprintf("chưa đủ để so: %d task hỏi graph khi lập plan, %d task không (cần ít nhất %d mỗi bên)",
			u.With.Tasks, u.Without.Tasks, minTasks), w-4)) + "\n")
	}
	if u.With.Tasks >= minTasks && u.Without.Tasks >= minTasks {
		b.WriteString("\n" + titleStyle.Render("Lượt lập plan: có graph vs không") + "\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("%-14s %5s %9s %8s %9s %8s", "", "task", "tra cứu", "subagent", "giá API", "thời gian")) + "\n")
		row := func(name string, e ExploreRow) {
			fmt.Fprintf(&b, "%-14s %5d %9.1f %8.1f %9s %8s\n", name, e.Tasks, e.Lookups, e.Explorers, fmt.Sprintf("$%.2f", e.PlanCost), fmt.Sprintf("%.0fp", e.PlanMinutes))
		}
		row("hỏi graph", ExploreRow(u.With))
		row("không hỏi", ExploreRow(u.Without))
		if u.Without.Lookups > 0 {
			d := (u.With.Lookups - u.Without.Lookups) / u.Without.Lookups * 100
			msg := fmt.Sprintf("tra cứu %+.0f%%", d)
			if u.Without.PlanCost > 0 {
				msg += fmt.Sprintf(" · giá API plan %+.0f%%", (u.With.PlanCost-u.Without.PlanCost)/u.Without.PlanCost*100)
			}
			style := lipgloss.NewStyle().Foreground(clr(cOk))
			if d > 0 {
				style = warnStyle
			}
			b.WriteString(style.Render(msg) + "\n")
		}
		b.WriteString(dimStyle.Render(wrap("chia theo lượt lập plan (lượt 1) có hỏi graph không · so sánh thô, task khác nhau về độ khó · tra cứu = Grep, Glob, Read, rg, cat…, không tính lệnh bị chặn", w-4)) + "\n")
	}
	if len(u.Recent) > 0 {
		b.WriteString("\n" + dimStyle.Render("gần đây:") + "\n")
		for _, r := range u.Recent[:min(3, len(u.Recent))] {
			b.WriteString(dimStyle.Render(truncate("  "+r, w-4)) + "\n")
		}
	}
	return card("Graphify", cRule, b.String(), w)
}

// foldCard shows how tasks follow "When to fold subagents into your main
// agent loop": how many stay in the main loop, which subagents are spawned,
// whether the skill's rules hold (no subagent on Light tasks, a reviewer
// before the pr gate on Standard+), and how tasks with subagents compare.
func (m Model) foldCard(w int) string {
	const title = "Subagent: vòng chính hay tách riêng"
	fs, err := m.d.Store.FoldUsage(time.Now().AddDate(0, 0, -30))
	if err != nil {
		return card(title, cErr, errStyle.Render(err.Error()), w)
	}
	var b strings.Builder
	if fs.Tasks == 0 {
		b.WriteString(dimStyle.Render("Chưa có task nào chạy trong 30 ngày."))
		return card(title, cRule, b.String(), w)
	}
	big := fg(cBright)
	green := lipgloss.NewStyle().Foreground(clr(cOk))
	calls := 0
	var kinds []string
	for k, n := range fs.ByType {
		calls += n
		kinds = append(kinds, fmt.Sprintf("%s %d", k, n))
	}
	sort.Strings(kinds)
	fmt.Fprintf(&b, "%s task làm trọn trong vòng chính (%d%%) · %s lần gọi subagent\n",
		big.Render(fmt.Sprintf("%d/%d", fs.Folded, fs.Tasks)), fs.Folded*100/fs.Tasks, big.Render(fmt.Sprint(calls)))
	if len(kinds) > 0 {
		b.WriteString(dimStyle.Render(strings.Join(kinds, " · ")) + "\n")
	}

	b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("%-11s %5s %8s %9s %8s %5s %8s", "level", "task", "có sub", "sub/task", "giá API", "lượt", "từ chối")) + "\n")
	for _, l := range fs.Levels {
		name := levelLabels[l.Level]
		if name == "" {
			name = "chưa đánh giá"
		}
		fmt.Fprintf(&b, "%-11s %5d %8s %9.1f %8s %5.1f %8.1f\n", truncate(name, 11), l.Tasks, fmt.Sprintf("%d", l.WithSub),
			float64(l.Subagents)/float64(l.Tasks), fmt.Sprintf("$%.2f", l.Cost), l.Runs, l.Rejections)
	}

	b.WriteString("\n" + titleStyle.Render("Tuân thủ nguyên tắc") + "\n")
	if len(fs.LightSpawned) == 0 {
		b.WriteString(okMark + " task Nhẹ không gọi subagent\n")
	} else {
		b.WriteString(errMark + " " + warnStyle.Render(fmt.Sprintf("%d task Nhẹ vẫn gọi subagent (nên tự làm): %s", len(fs.LightSpawned),
			strings.Join(fs.LightSpawned[:min(3, len(fs.LightSpawned))], ", "))) + "\n")
	}
	switch {
	case fs.NeedReview == 0:
		b.WriteString(offMark + dimStyle.Render(" chưa có task Trung bình trở lên tới cổng pr") + "\n")
	case fs.Reviewed == fs.NeedReview:
		b.WriteString(okMark + fmt.Sprintf(" %d/%d task Trung bình+ có reviewer trước cổng pr\n", fs.Reviewed, fs.NeedReview))
	default:
		line := fmt.Sprintf(" %d/%d task Trung bình+ có reviewer trước cổng pr", fs.Reviewed, fs.NeedReview)
		b.WriteString(warnMark + warnStyle.Render(line) + "\n")
		b.WriteString(dimStyle.Render(truncate("  thiếu: "+strings.Join(fs.Unreviewed[:min(3, len(fs.Unreviewed))], ", "), w-4)) + "\n")
	}

	if fs.With.Tasks > 0 && fs.Without.Tasks > 0 {
		b.WriteString("\n" + titleStyle.Render("Hiệu quả (task Trung bình+)") + "\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("%-15s %5s %8s %5s %12s", "", "task", "giá API", "lượt", "gate bị từ chối")) + "\n")
		row := func(name string, g store.FoldGroup) {
			rate := "-"
			if g.Decided > 0 {
				rate = fmt.Sprintf("%d%% (%d/%d)", g.Rejected*100/g.Decided, g.Rejected, g.Decided)
			}
			fmt.Fprintf(&b, "%-15s %5d %8s %5.1f %12s\n", name, g.Tasks, fmt.Sprintf("$%.2f", g.Cost), g.Runs, rate)
		}
		row("có subagent", fs.With)
		row("trọn vòng chính", fs.Without)
		if fs.With.Decided > 0 && fs.Without.Decided > 0 {
			rw := float64(fs.With.Rejected) / float64(fs.With.Decided)
			ro := float64(fs.Without.Rejected) / float64(fs.Without.Decided)
			msg := fmt.Sprintf("có subagent: tỉ lệ bị từ chối %+.0f điểm %%, chi phí %+.0f%%", (rw-ro)*100, (fs.With.Cost-fs.Without.Cost)/fs.Without.Cost*100)
			style := green
			if rw > ro {
				style = warnStyle
			}
			b.WriteString(style.Render(msg) + "\n")
		}
		b.WriteString(dimStyle.Render(wrap("so sánh thô, task có subagent thường khó hơn · xem cùng bảng theo level", w-4)) + "\n")
	}
	return card(title, cRule, b.String(), w)
}

// ExploreRow is store.ExploreStats (local name for the table).
type ExploreRow = store.ExploreStats

func (m Model) statsCard(w int) string {
	var b strings.Builder
	st, err := m.d.Store.Stats()
	if err != nil {
		return card("Hoạt động", cErr, errStyle.Render(err.Error()), w)
	}
	big := fg(cBright)
	fmt.Fprintf(&b, "%s %s   %s %s   %s %s\n",
		big.Render(fmt.Sprint(st.TasksDone7d)), dimStyle.Render("task xong"),
		big.Render(fmt.Sprint(st.Runs7d)), dimStyle.Render("lượt chạy"),
		big.Render(fmtUSD(st.Cost7d)), dimStyle.Render("giá API · hôm nay "+fmtUSD(st.CostToday)))
	if len(st.Gates) > 0 {
		b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("%-8s %8s %9s %8s %12s", "cổng", "duyệt", "đã quyết", "đang chờ", "bạn trả lời")) + "\n")
		for _, g := range st.Gates {
			rate := "-"
			if g.ApprovalRate >= 0 {
				rate = fmt.Sprintf("%.0f%%", g.ApprovalRate*100)
			}
			pending := fmt.Sprintf("%8d", g.Pending)
			if g.Pending > 0 {
				pending = warnStyle.Render(pending)
			}
			fmt.Fprintf(&b, "%-8s %8s %9s %s %12s\n", g.Gate, rate, fmt.Sprintf("%d/%d", g.Approved, g.Approved+g.Rejected), pending,
				fmt.Sprintf("~%.0f phút", g.AvgWaitMinutes))
		}
	}
	if len(st.TopDenials) > 0 {
		b.WriteString("\n" + warnMark + " " + dimStyle.Render("agent bị chặn nhiều nhất") + "\n")
		for _, d := range st.TopDenials[:min(3, len(st.TopDenials))] {
			b.WriteString("  " + truncate(d, w-6) + "\n")
		}
	}
	if st.OutboxPending+st.OutboxFailed > 0 {
		fmt.Fprintf(&b, "%s báo cáo về ticket: %d đang chờ gửi lại, %d thất bại\n", warnMark, st.OutboxPending, st.OutboxFailed)
	}
	return card("7 ngày qua", cRule, b.String(), w)
}

func (m Model) projectsCard(w int) string {
	var b strings.Builder
	names := m.d.Config.ProjectNames()
	nw := 0
	for _, n := range names {
		nw = max(nw, lipgloss.Width(n))
	}
	for _, name := range names {
		p := m.d.Config.Projects[name]
		fmt.Fprintf(&b, "%s %s  %s\n", titleStyle.Render(padRight(name, nw)),
			dimStyle.Render(fmt.Sprintf("%2d repo", len(p.Repos))), truncate(p.Name, w-nw-14))
	}
	b.WriteString("\n" + dimStyle.Render(truncate(m.d.Config.Path, w-4)))
	return card("Dự án", cRule, b.String(), w)
}

func (m Model) logCard(w int) string {
	var b strings.Builder
	lines := m.d.Daemon.Logs()
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	if len(lines) == 0 {
		b.WriteString(dimStyle.Render("(chưa có)"))
	}
	for _, l := range lines {
		l = truncate(l, w-4)
		ll := strings.ToLower(l)
		switch {
		case strings.Contains(ll, "error") || strings.Contains(ll, "failed") || strings.Contains(ll, "lỗi") || strings.Contains(ll, "panic"):
			l = errStyle.Render(l)
		case strings.Contains(ll, "scan "):
			l = accentStyle.Render(l)
		}
		b.WriteString(l + "\n")
	}
	return card("Log của daemon", cRule, b.String(), w)
}

// The Status tab has three sections, switched with ←/→: what taskpilot
// runs on (connections, daemon, gates, projects, the daemon log), the token
// usage over time, and how the agents work (graphify, subagents).
type statusSection int

const (
	secOverview statusSection = iota
	secUsage
	secAgents
)

var statusSections = []string{"Tổng quan", "Usage", "Agent"}

func (m *Model) refreshStatus() {
	w := max(40, m.stVP.Width)
	wide := w >= 120
	lw := w * 11 / 20
	rw := w - lw - 1
	out := []string{m.statusSectionsLine(w)}
	switch m.stSec {
	case secOverview:
		if wide {
			out = append(out, columns(
				[]string{m.connectionsCard(lw), m.projectsCard(lw)},
				[]string{m.daemonCard(rw), m.statsCard(rw)}))
		} else {
			out = append(out, m.connectionsCard(w), m.daemonCard(w), m.statsCard(w), m.projectsCard(w))
		}
		if m.d.Daemon.Logs != nil {
			out = append(out, m.logCard(w))
		}
	case secUsage:
		m.loadUsage()
		out = append(out, m.usageContent(w, time.Now()))
	case secAgents:
		if wide {
			out = append(out, columns([]string{m.graphCard(lw)}, []string{m.foldCard(rw)}))
		} else {
			out = append(out, m.graphCard(w), m.foldCard(w))
		}
	}
	y := m.stVP.YOffset
	m.stVP.SetContent(strings.Join(out, "\n"))
	m.stVP.SetYOffset(y)
}

// statusSectionsLine names the sections, the current one bright.
func (m Model) statusSectionsLine(w int) string {
	var parts []string
	for i, name := range statusSections {
		if statusSection(i) == m.stSec {
			parts = append(parts, activeTabStyle.Render(name))
		} else {
			parts = append(parts, tabStyle.Render(name))
		}
	}
	return " " + strings.Join(parts, "   ") + "\n"
}

// columns sets two stacks of cards side by side, the shorter stack's last
// card stretched so both end on the same row.
func columns(left, right []string) string {
	l, r := strings.Join(left, "\n"), strings.Join(right, "\n")
	lh, rh := lipgloss.Height(l), lipgloss.Height(r)
	switch {
	case lh < rh:
		left[len(left)-1] = stretch(left[len(left)-1], rh-lh)
		l = strings.Join(left, "\n")
	case rh < lh:
		right[len(right)-1] = stretch(right[len(right)-1], lh-rh)
		r = strings.Join(right, "\n")
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, l, " ", r)
}

// stretch adds n empty rows at the bottom of a card, inside its frame.
func stretch(card string, n int) string {
	lines := strings.Split(card, "\n")
	bottom := lines[len(lines)-1]
	empty := strings.NewReplacer("╰", "│", "╯", "│", "─", " ").Replace(bottom)
	fill := make([]string, n)
	for i := range fill {
		fill[i] = empty
	}
	return strings.Join(append(append(lines[:len(lines)-1:len(lines)-1], fill...), bottom), "\n")
}

// statusKeys is the footer of the Status tab: the section's own keys.
func (m Model) statusKeys() string {
	switch m.stSec {
	case secUsage:
		return "←/→ mục · [ ] khoảng thời gian · c chia giờ/ngày/tuần/tháng · j/k cuộn · r làm mới"
	case secAgents:
		return "←/→ mục · j/k cuộn · r làm mới · :graphify build dựng lại graph"
	}
	return "←/→ mục · l Hasaki · t dán JWT · s/S Splunk (S: token 90 ngày) · a Argo CD · v Vault · K Kafka · b Bitbucket · d doctor · r làm mới"
}

func (m Model) statusKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	switch key {
	case "left", "right":
		n := len(statusSections)
		d := map[bool]int{true: n - 1, false: 1}[key == "left"]
		m.stSec = statusSection((int(m.stSec) + d) % n)
		m.stVP.GotoTop()
		m.refreshStatus()
		return m, nil
	}
	if m.stSec == secUsage && m.usageKey(key) {
		m.refreshStatus()
		return m, nil
	}
	switch key {
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
	case "v":
		return m.runCommand("auth vault")
	case "K":
		return m.runCommand("auth kafka")
	case "S":
		return m.runCommand("auth splunk -create")
	case "d":
		return m.runCommand("doctor")
	case "r":
		m.refreshBanner()
		m.refreshStatus()
		m.msg = "đã làm mới"
	case "g", "home":
		m.stVP.GotoTop()
	case "G", "end":
		m.stVP.GotoBottom()
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
