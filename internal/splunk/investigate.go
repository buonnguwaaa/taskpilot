package splunk

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// InvestigationMarker opens the body of a production investigation ticket;
// the workflow skill keys on it.
const InvestigationMarker = "Task type: production investigation"

// isoLayout is the absolute time form splunk_search accepts.
const isoLayout = "2006-01-02T15:04:05"

var snapRe = regexp.MustCompile(`^(-(\d+)([smhdw]))?@([smhdw])$`)

// ResolveTime turns a Splunk time (now, -4h, @d, -1d@d, ISO) into a time at
// now; ok=false for forms it does not know.
func ResolveTime(s string, now time.Time) (time.Time, bool) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "now":
		return now, true
	}
	if t, err := time.ParseInLocation(isoLayout, s, now.Location()); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, now.Location()); err == nil {
		return t, true
	}
	if d, ok := Lookback(s); ok && !strings.Contains(s, "@") {
		return now.Add(-d), true
	}
	m := snapRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	t := now
	if m[1] != "" {
		n, _ := strconv.Atoi(m[2])
		switch m[3] {
		case "d":
			t = t.AddDate(0, 0, -n)
		case "w":
			t = t.AddDate(0, 0, -7*n)
		default:
			d, _ := Lookback("-" + m[2] + m[3])
			t = t.Add(-d)
		}
	}
	y, mo, d := t.Date()
	switch m[4] {
	case "d":
		return time.Date(y, mo, d, 0, 0, 0, 0, t.Location()), true
	case "w":
		day := time.Date(y, mo, d, 0, 0, 0, 0, t.Location())
		return day.AddDate(0, 0, -int(day.Weekday())), true
	case "h":
		return t.Truncate(time.Hour), true
	case "m":
		return t.Truncate(time.Minute), true
	}
	return t.Truncate(time.Second), true
}

// AbsoluteWindow pins a relative window to absolute ISO times, so a search
// run later (by an agent, hours after) covers the same events. It returns the
// inputs unchanged when they cannot be resolved.
func AbsoluteWindow(earliest, latest string, now time.Time) (string, string) {
	from, ok1 := ResolveTime(earliest, now)
	to, ok2 := ResolveTime(latest, now)
	if !ok1 || !ok2 {
		return earliest, latest
	}
	return from.Format(isoLayout), to.Format(isoLayout)
}

// Investigation is what the operator saw, turned into a ticket for an agent
// that investigates the production issue before anything is changed.
type Investigation struct {
	Project string
	Symptom string // in the operator's words; may be empty when a group says it all
	Service string // container, "" when unknown
	IDs     string // order / transaction codes, request_id… given by the operator
	Filter  string // the project's production filter (added to every search)

	Earliest, Latest string // absolute ISO (see AbsoluteWindow)

	Group   *ErrorGroup // the error group it started from, if any
	SPL     string      // the search it started from, if any
	Samples []string    // a few log lines as shown in the TUI
}

// Title is a short ticket title.
func (in Investigation) Title() string {
	what := strings.TrimSpace(in.Symptom)
	if i := strings.IndexByte(what, '\n'); i >= 0 {
		what = what[:i]
	}
	if what == "" && in.Group != nil {
		what = in.Group.Signature
	}
	if what == "" {
		what = "lỗi production"
	}
	svc := ""
	if in.Service != "" {
		svc = in.Service + ": "
	}
	return clip("Điều tra prod "+svc+strings.TrimSpace(what), 100)
}

// Body is the ticket description: the evidence the operator had, where to
// search, and what the investigation must deliver.
func (in Investigation) Body() string {
	var b strings.Builder
	b.WriteString(InvestigationMarker + " (điều tra sự cố production)\n\n")
	b.WriteString("## Triệu chứng\n")
	if s := strings.TrimSpace(in.Symptom); s != "" {
		b.WriteString(s + "\n")
	}
	if g := in.Group; g != nil {
		fmt.Fprintf(&b, "Nhóm lỗi trên Splunk: service `%s`, %d lần", g.Service, g.Count)
		if !g.First.IsZero() {
			fmt.Fprintf(&b, " (lần đầu %s, gần nhất %s)", g.First.Local().Format("2006-01-02 15:04:05"), g.Last.Local().Format("2006-01-02 15:04:05"))
		}
		b.WriteString(".\n")
		fmt.Fprintf(&b, "- Thông điệp (đã che số/mã): %s\n", g.Signature)
		if g.Sample != "" && g.Sample != g.Signature {
			fmt.Fprintf(&b, "- Ví dụ thật: %s\n", clip(g.Sample, 500))
		}
		for _, kv := range [][2]string{{"caller", g.Caller}, {"prefix (hàm)", g.Prefix}, {"request_id gần nhất", g.RequestID}} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "- %s: %s\n", kv[0], kv[1])
			}
		}
		if st := strings.TrimSpace(g.Stacktrace); st != "" {
			fmt.Fprintf(&b, "- Stacktrace (rút gọn):\n```\n%s\n```\n", clip(st, 1500))
		}
	}
	b.WriteString("\n## Phạm vi\n")
	fmt.Fprintf(&b, "- Dự án: %s", in.Project)
	if in.Service != "" {
		fmt.Fprintf(&b, ", service `%s`", in.Service)
	}
	b.WriteString("\n")
	window := in.Earliest + " → " + in.Latest
	if in.Latest == "" || in.Latest == "now" {
		window = in.Earliest + " → lúc tạo task"
	}
	fmt.Fprintf(&b, "- Khoảng thời gian (giờ địa phương): %s. Dùng đúng các mốc này với splunk_search (earliest/latest), mở rộng nếu cần tìm lúc bắt đầu.\n", window)
	if ids := strings.TrimSpace(in.IDs); ids != "" {
		fmt.Fprintf(&b, "- Mã liên quan do operator cung cấp: %s\n", ids)
	}
	if in.Filter != "" {
		fmt.Fprintf(&b, "- Log production của dự án: `%s` (splunk_search tự thêm bộ lọc của dự án).\n", in.Filter)
	}
	if spl := strings.TrimSpace(in.SPL); spl != "" {
		fmt.Fprintf(&b, "\n## Truy vấn đã chạy\n```\n%s\n```\n", spl)
	}
	if len(in.Samples) > 0 {
		b.WriteString("\n## Dòng log mẫu\n```\n")
		for _, l := range in.Samples {
			b.WriteString(clip(l, 300) + "\n")
		}
		b.WriteString("```\n")
	}
	b.WriteString(`
## Cần làm (điều tra trước, chưa sửa)
1. Dựng lại diễn biến từ log: lỗi bắt đầu từ lúc nào, tần suất, service nào gọi service nào (theo request_id / mã giao dịch qua các service).
2. Đọc code tại caller/prefix để hiểu lỗi phát sinh ở đâu và vì sao; đối chiếu dữ liệu trên DB QC (db_query) và log QC nếu giúp tái hiện.
3. Kết luận nguyên nhân gốc kèm bằng chứng (dòng log, path:line, kết quả truy vấn), và ảnh hưởng: bao nhiêu request/đơn/giao dịch, khách có bị ảnh hưởng không.
4. Gửi báo cáo ở cổng plan: nguyên nhân, ảnh hưởng, đề xuất sửa (hoặc xử lý vận hành), rủi ro. Chỉ sửa code sau khi plan được duyệt.
5. Nếu không cần sửa code (lỗi phía đối tác, dữ liệu, cấu hình, lỗi dự kiến), kết thúc bằng complete_task với báo cáo đó.
`)
	return b.String()
}
