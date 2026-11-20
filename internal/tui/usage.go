package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// The Usage section of the Status tab: tokens and cost over a period, cut by hour, day, week or
// month, and broken down by model, project, source and task. Task runs come
// from runs; the helper calls (drafting, work logs, SPL, grouping errors,
// reflect…) from claude_calls, recorded since schema v17.

type usagePeriod struct {
	label  string
	bucket store.Bucket
	span   func(now time.Time, first time.Time) (from, to time.Time)
}

func dayStart(t time.Time) time.Time {
	y, mo, d := t.Local().Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
}

var usagePeriods = []usagePeriod{
	{"Hôm nay", store.ByHour, func(now, _ time.Time) (time.Time, time.Time) {
		return dayStart(now), dayStart(now).AddDate(0, 0, 1)
	}},
	{"Hôm qua", store.ByHour, func(now, _ time.Time) (time.Time, time.Time) {
		return dayStart(now).AddDate(0, 0, -1), dayStart(now)
	}},
	{"7 ngày", store.ByDay, func(now, _ time.Time) (time.Time, time.Time) {
		return dayStart(now).AddDate(0, 0, -6), dayStart(now).AddDate(0, 0, 1)
	}},
	{"30 ngày", store.ByDay, func(now, _ time.Time) (time.Time, time.Time) {
		return dayStart(now).AddDate(0, 0, -29), dayStart(now).AddDate(0, 0, 1)
	}},
	{"Tháng này", store.ByDay, func(now, _ time.Time) (time.Time, time.Time) {
		m := store.ByMonth.Start(now)
		return m, m.AddDate(0, 1, 0)
	}},
	{"Tháng trước", store.ByDay, func(now, _ time.Time) (time.Time, time.Time) {
		m := store.ByMonth.Start(now)
		return m.AddDate(0, -1, 0), m
	}},
	{"12 tuần", store.ByWeek, func(now, _ time.Time) (time.Time, time.Time) {
		w := store.ByWeek.Start(now)
		return w.AddDate(0, 0, -77), w.AddDate(0, 0, 7)
	}},
	{"Tất cả", store.ByMonth, func(now, first time.Time) (time.Time, time.Time) {
		if first.IsZero() {
			first = now
		}
		return store.ByMonth.Start(first), store.ByMonth.Start(now).AddDate(0, 1, 0)
	}},
}

var bucketNames = map[store.Bucket]string{store.ByHour: "giờ", store.ByDay: "ngày", store.ByWeek: "tuần", store.ByMonth: "tháng"}

type usageTab struct {
	period int
	bucket store.Bucket
	custom bool // bucket chosen with c, not the period's own
	sum    *store.UsageSummary
	err    error
}

// fullWidthTab: the tab is cards over the whole width (no list, no divider).
func (m Model) fullWidthTab() bool { return m.tab == tabStatus }

// loadUsage reads the chosen period's usage.
func (m *Model) loadUsage() {
	p := usagePeriods[m.usage.period]
	from, to := p.span(time.Now(), m.d.Store.FirstUsage())
	b := p.bucket
	if m.usage.custom {
		b = m.usage.bucket
	}
	rows, err := m.d.Store.UsageRows(from, to)
	m.usage.err = err
	if err == nil {
		s := store.SummarizeUsage(rows, from, to, b)
		m.usage.sum = &s
	}
}

// usageKey handles the Usage section's own keys; handled=false: not one.
func (m *Model) usageKey(k string) bool {
	switch k {
	case "[":
		m.usage.period = (m.usage.period + len(usagePeriods) - 1) % len(usagePeriods)
		m.usage.custom = false
	case "]":
		m.usage.period = (m.usage.period + 1) % len(usagePeriods)
		m.usage.custom = false
	case "c":
		cur := usagePeriods[m.usage.period].bucket
		if m.usage.custom {
			cur = m.usage.bucket
		}
		m.usage.bucket, m.usage.custom = (cur+1)%4, true
	default:
		return false
	}
	m.stVP.GotoTop()
	return true
}

// fmtTok writes a token count short: 950, 12.3K, 4.5M, 1.20B.
func fmtTok(n int64) string {
	f := float64(n)
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1e6:
		return fmt.Sprintf("%.1fK", f/1e3)
	case n < 1e9:
		return fmt.Sprintf("%.1fM", f/1e6)
	}
	return fmt.Sprintf("%.2fB", f/1e9)
}

// fmtUSD writes dollars with thousands separators: $2,775.89.
func fmtUSD(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	whole, cents, _ := strings.Cut(s, ".")
	neg := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if neg {
		whole = "-" + whole
	}
	return "$" + whole + "." + cents
}

var weekdays = []string{"CN", "T2", "T3", "T4", "T5", "T6", "T7"}

// bucketLabel names a bucket: "14:00", "T5 20/11", "tuần 17/11", "11/2026".
func bucketLabel(b store.Bucket, t time.Time) string {
	switch b {
	case store.ByHour:
		return t.Format("02/01 15:00")
	case store.ByWeek:
		return "tuần " + t.Format("02/01")
	case store.ByMonth:
		return t.Format("01/2006")
	}
	return weekdays[t.Weekday()] + " " + t.Format("02/01")
}

func (m Model) usageContent(w int, now time.Time) string {
	if m.usage.err != nil {
		return card("Usage", cErr, errStyle.Render(m.usage.err.Error()), w)
	}
	s := m.usage.sum
	if s == nil {
		return ""
	}
	var out []string
	out = append(out, m.usagePeriodsLine(w), m.usageTotalCard(s, w), m.usageBucketsCard(s, w, now))
	if w >= 120 {
		cw := (w - 2) / 3
		row := lipgloss.JoinHorizontal(lipgloss.Top,
			namedCard("Theo model", s.Models, cw, -1), " ",
			namedCard("Theo project", s.Projects, cw, -1), " ",
			namedCard("Theo nguồn", s.Sources, w-2*cw-2, -1))
		out = append(out, row)
	} else {
		out = append(out, namedCard("Theo model", s.Models, w, 0), namedCard("Theo project", s.Projects, w, 0),
			namedCard("Theo nguồn", s.Sources, w, 0))
	}
	out = append(out, namedCard("Task tốn nhiều token nhất", s.Tasks, w, 15))
	return strings.Join(out, "\n")
}

func (m Model) usagePeriodsLine(w int) string {
	var parts []string
	for i, p := range usagePeriods {
		if i == m.usage.period {
			parts = append(parts, activeTabStyle.Render(p.label))
		} else {
			parts = append(parts, dimStyle.Render(p.label))
		}
	}
	return " " + ansi.Truncate(strings.Join(parts, dimStyle.Render("  ")), w-2, "…")
}

func (m Model) usageTotalCard(s *store.UsageSummary, w int) string {
	big := fg(cBright)
	t := s.Total
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s   %s %s   %s %s   %s %s\n",
		dimStyle.Render("tổng token"), big.Render(fmtTok(t.Total())),
		dimStyle.Render("giá API"), big.Render(fmtUSD(t.CostUSD)),
		dimStyle.Render("lượt chạy task"), big.Render(fmt.Sprint(s.Runs.Calls)),
		dimStyle.Render("lệnh phụ"), big.Render(fmt.Sprint(s.Helpers.Calls)))
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s %s   %s %s   %s %s   %s %s\n",
		dimStyle.Render("input mới"), fmtTok(t.Input),
		dimStyle.Render("output"), fmtTok(t.Output),
		dimStyle.Render("cache đọc"), fmtTok(t.CacheRead),
		dimStyle.Render("cache ghi"), fmtTok(t.CacheWrite))
	if in := t.Input + t.CacheRead + t.CacheWrite; in > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("%.0f%% token đầu vào đọc từ cache · task %s · lệnh phụ %s",
			float64(t.CacheRead)*100/float64(in), fmtUSD(s.Runs.CostUSD), fmtUSD(s.Helpers.CostUSD))) + "\n")
	}
	b.WriteString(dimStyle.Render("giá API = token × giá niêm yết của Anthropic; dùng gói subscription thì không trả số này, chỉ để so độ nặng"))
	first, last := s.From.Format("02/01/2006"), s.To.Add(-time.Second).Format("02/01/2006")
	title := usagePeriods[m.usage.period].label + " · " + first
	if last != first {
		title += " → " + last
	}
	return card(title, cRule, b.String(), w)
}

// usageBucketsCard is one row per bucket: a bar of its tokens, then the figures.
func (m Model) usageBucketsCard(s *store.UsageSummary, w int, now time.Time) string {
	var rows []store.Named
	for _, bk := range s.Buckets {
		t, _ := time.Parse(time.RFC3339, bk.Name)
		if t.After(now) {
			break // the rest of today, of this month…
		}
		rows = append(rows, bk)
	}
	// Empty buckets before the first use say nothing: the night hours, the
	// weeks before taskpilot was installed.
	for len(rows) > 1 && rows[0].Calls == 0 && (s.Bucket == store.ByHour || m.usage.period >= 6) {
		rows = rows[1:]
	}
	var peak int64
	for _, r := range rows {
		peak = max(peak, r.Total())
	}
	const labelW, figW = 12, 47
	barW := max(6, w-4-labelW-figW-2)
	cur := s.Bucket.Start(now)
	var b strings.Builder
	b.WriteString(dimStyle.Render(fmt.Sprintf("%-*s %-*s %9s %9s %9s %10s %4s", labelW, "", barW, "", "tổng", "output", "cache ghi", "giá API", "lượt")) + "\n")
	for _, r := range rows {
		t, _ := time.Parse(time.RFC3339, r.Name)
		label := fmt.Sprintf("%-*s", labelW, bucketLabel(s.Bucket, t))
		n := 0
		if peak > 0 {
			n = int(float64(r.Total()) / float64(peak) * float64(barW))
			if r.Total() > 0 {
				n = max(1, n)
			}
		}
		barColor := cSub
		if t.Equal(cur) {
			barColor, label = cRun, fg(cBright).Render(label)
		} else {
			label = dimStyle.Render(label)
		}
		bar := fg(barColor).Render(strings.Repeat("█", n)) + strings.Repeat(" ", barW-n)
		figs := fmt.Sprintf("%9s %9s %9s %10s %4d", fmtTok(r.Total()), fmtTok(r.Output), fmtTok(r.CacheWrite), fmtUSD(r.CostUSD), r.Calls)
		if r.Calls == 0 {
			figs = dimStyle.Render(fmt.Sprintf("%9s %9s %9s %10s %4s", "-", "-", "-", "-", "-"))
		}
		b.WriteString(label + " " + bar + " " + figs + "\n")
	}
	if len(rows) == 0 {
		b.WriteString(dimStyle.Render("chưa có gì trong khoảng này"))
	}
	return card("Theo "+bucketNames[s.Bucket], cRule, b.String(), w)
}

// namedCard is a ranked table of sums; limit 0: all of them, -1: all of
// them in a narrow card (no cache column).
func namedCard(title string, list []store.Named, w, limit int) string {
	compact := limit < 0
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	figW := 9*3 + 10 + 5 + 5
	if compact {
		figW -= 10
	}
	nameW := max(8, w-4-figW)
	var b strings.Builder
	head := fmt.Sprintf("%-*s %9s %9s %9s %10s %5s", nameW, "", "tổng", "output", "cache đọc", "giá API", "lượt")
	if compact {
		head = fmt.Sprintf("%-*s %9s %9s %10s %5s", nameW, "", "tổng", "output", "giá API", "lượt")
	}
	b.WriteString(dimStyle.Render(head) + "\n")
	for _, n := range list {
		name := n.Name
		if n.Detail != "" {
			name += " " + dimStyle.Render(n.Detail)
		}
		name = ansi.Truncate(name, nameW, "…")
		if pad := nameW - lipgloss.Width(name); pad > 0 {
			name += strings.Repeat(" ", pad)
		}
		if compact {
			fmt.Fprintf(&b, "%s %9s %9s %10s %5d\n", name, fmtTok(n.Total()), fmtTok(n.Output), fmtUSD(n.CostUSD), n.Calls)
		} else {
			fmt.Fprintf(&b, "%s %9s %9s %9s %10s %5d\n", name, fmtTok(n.Total()), fmtTok(n.Output), fmtTok(n.CacheRead), fmtUSD(n.CostUSD), n.Calls)
		}
	}
	if len(list) == 0 {
		b.WriteString(dimStyle.Render("chưa có gì"))
	}
	return card(title, cRule, b.String(), w)
}
