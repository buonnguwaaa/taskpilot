package splunk

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- time presets -------------------------------------------------------------

// TimeRange is a named search window in Splunk's time syntax.
type TimeRange struct {
	Label    string
	Earliest string
	Latest   string // "" = now
}

// Presets are the windows offered in the TUI, shortest first.
var Presets = []TimeRange{
	{"15 phút", "-15m", ""},
	{"1 giờ", "-1h", ""},
	{"4 giờ", "-4h", ""},
	{"24 giờ", "-24h", ""},
	{"Hôm nay", "@d", ""},
	{"Hôm qua", "-1d@d", "@d"},
	{"7 ngày", "-7d", ""},
	{"30 ngày", "-30d", ""},
	{"Toàn bộ", "0", ""},
}

// PresetFor finds the preset with this earliest/latest, if any.
func PresetFor(earliest, latest string) (TimeRange, bool) {
	for _, p := range Presets {
		if p.Earliest == earliest && p.Latest == latest {
			return p, true
		}
	}
	return TimeRange{}, false
}

// Describe labels a window: the preset name, or "earliest → latest".
func Describe(earliest, latest string) string {
	if p, ok := PresetFor(earliest, latest); ok {
		return p.Label
	}
	if latest == "" || latest == "now" {
		return "từ " + earliest
	}
	return earliest + " → " + latest
}

// ---- query builder ------------------------------------------------------------

// Levels offered by the builder.
const (
	LevelError = "error"      // error and worse
	LevelWarn  = "warn+error" // warnings too
	LevelAny   = ""           // every level
)

// Query is a structured search the TUI turns into SPL.
type Query struct {
	Service   string // container name; "" = every service of the project
	Level     string // LevelError, LevelWarn or LevelAny
	Text      string // words or a quoted phrase the event must contain
	RequestID string
	// NoNoise drops lines that are not structured app logs (GORM SQL,
	// fmt.Print…), which carry no level field.
	NoNoise bool
}

// BuildSPL turns q into the base search (without the project filter, which
// Scope adds), newest first.
func BuildSPL(q Query) string {
	var parts []string
	if s := strings.TrimSpace(q.Service); s != "" {
		parts = append(parts, "kubernetes.container_name="+quoteIfNeeded(s))
	}
	switch q.Level {
	case LevelError:
		parts = append(parts, "(level=error OR level=fatal OR level=panic OR level=dpanic)")
	case LevelWarn:
		parts = append(parts, "(level=warn OR level=error OR level=fatal OR level=panic OR level=dpanic)")
	default:
		if q.NoNoise {
			parts = append(parts, "level=*")
		}
	}
	if id := strings.TrimSpace(q.RequestID); id != "" {
		parts = append(parts, "request_id="+quoteIfNeeded(id))
	}
	if t := strings.TrimSpace(q.Text); t != "" {
		if strings.HasPrefix(t, `"`) || strings.ContainsAny(t, "=()") {
			parts = append(parts, t) // already SPL-ish: the operator knows what they want
		} else {
			parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `\"`)+`"`)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "*")
	}
	return strings.Join(parts, " ")
}

func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, " \t\"=()") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// ---- error groups -------------------------------------------------------------

// GroupSPL counts error events by service and normalized message: numbers,
// UUIDs, hex and long tokens are masked so one bug is one group. The regexes
// avoid [] (rejected by Validate as subsearches).
func GroupSPL(service string, limit int) string {
	base := BuildSPL(Query{Service: service, Level: LevelError})
	if limit <= 0 {
		limit = 50
	}
	return base + sigEval +
		` | stats count, min(_time) as first, max(_time) as last, latest(m) as sample, latest(caller) as caller,` +
		` latest(prefix) as prefix, latest(request_id) as request_id, latest(stacktrace) as stacktrace by service, sig` +
		` | sort - count | head ` + strconv.Itoa(limit)
}

// sigEval computes service and sig (the message with numbers and codes
// masked): the key of an error group.
const sigEval = ` | eval service=coalesce('kubernetes.container_name', "?")` +
	` | eval m=coalesce(message, msg, error, _raw)` +
	` | eval sig=replace(m, "\w{8}-\w{4}-\w{4}-\w{4}-\w{12}", "<uuid>")` +
	` | eval sig=replace(sig, "0x\w+", "<hex>")` +
	` | eval sig=replace(sig, "\w*\d\w*\d\w*\d\w*", "<id>")` +
	` | eval sig=replace(sig, "\d+", "<n>")` +
	` | eval sig=substr(sig, 1, 160)`

// linkSep separates service and signature in a LinkSPL group key.
const linkSep = " ¦ "

// LinkSPL finds error groups that happen in the same flow: errors logged
// with the same request_id, or the same transaction code (transaction_code
// field, or an HSK-… code in the line). Each row is a set of groups and how
// many requests/transactions they share.
func LinkSPL(service string, limit int) string {
	base := BuildSPL(Query{Service: service, Level: LevelError})
	if limit <= 0 {
		limit = 200
	}
	return base + sigEval +
		` | rex field=_raw "(?<hsk_code>HSK-\w+)"` +
		` | eval key=coalesce(request_id, transaction_code, hsk_code)` +
		` | where isnotnull(key) AND key!=""` +
		` | eval gid=service."` + linkSep + `".sig` +
		` | stats dc(gid) as n, values(gid) as gids by key` +
		` | where n>1 AND n<=8` +
		` | stats count as shared by gids` +
		` | sort - shared | head ` + strconv.Itoa(limit)
}

// Link is a set of error groups (service, signature) seen in the same
// requests or transactions, Shared times.
type Link struct {
	Groups [][2]string
	Shared int
}

// ParseLinks reads LinkSPL rows.
func ParseLinks(rows []Event) []Link {
	var out []Link
	for _, r := range rows {
		var ids []string
		switch v := r["gids"].(type) {
		case []any:
			for _, x := range v {
				ids = append(ids, str(x))
			}
		case string:
			ids = strings.Split(v, "\n")
		}
		var l Link
		for _, id := range ids {
			svc, sig, ok := strings.Cut(id, linkSep)
			if ok {
				l.Groups = append(l.Groups, [2]string{svc, sig})
			}
		}
		l.Shared, _ = strconv.Atoi(str(r["shared"]))
		if len(l.Groups) > 1 && l.Shared > 0 {
			out = append(out, l)
		}
	}
	return out
}

// ErrorGroup is one row of GroupSPL.
type ErrorGroup struct {
	Service     string
	Signature   string // normalized message
	Count       int
	First, Last time.Time
	Sample      string // one real message
	Caller      string
	Prefix      string
	RequestID   string
	Stacktrace  string
}

// ParseGroups reads GroupSPL rows, most frequent first.
func ParseGroups(rows []Event) []ErrorGroup {
	var out []ErrorGroup
	for _, r := range rows {
		g := ErrorGroup{
			Service: str(r["service"]), Signature: str(r["sig"]), Sample: str(r["sample"]),
			Caller: str(r["caller"]), Prefix: str(r["prefix"]), RequestID: str(r["request_id"]), Stacktrace: str(r["stacktrace"]),
		}
		g.Count, _ = strconv.Atoi(str(r["count"]))
		g.First, g.Last = epoch(r["first"]), epoch(r["last"])
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// epoch reads a Splunk _time aggregate (epoch seconds, possibly fractional).
func epoch(v any) time.Time {
	f, err := strconv.ParseFloat(str(v), 64)
	if err != nil || f <= 0 {
		return time.Time{}
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9))
}

// DrillSPL finds the events of one group: its service, error level, its
// caller (the exact log line in the code) when known, and the longest plain
// phrase of its signature (the masked parts vary between events).
func DrillSPL(g ErrorGroup) string {
	spl := BuildSPL(Query{Service: g.Service, Level: LevelError})
	if c := strings.TrimSpace(g.Caller); c != "" && !strings.ContainsAny(c, `"|[]`) {
		spl += ` caller="` + c + `"`
	}
	if t := drillTerms(g.Signature); t != "" {
		spl += ` "` + t + `"`
	}
	return spl
}

// drillTerms keeps the longest run of the signature without masked parts,
// quotes, = or brackets: a phrase Splunk matches literally.
func drillTerms(sig string) string {
	best := ""
	for _, part := range strings.FieldsFunc(sig, func(r rune) bool { return strings.ContainsRune(`<>"=()[]|\`+"`", r) }) {
		part = strings.Trim(strings.TrimSpace(part), ` :,;.'`)
		switch part {
		case "uuid", "hex", "id", "n":
			continue
		}
		if len(part) > len(best) {
			best = part
		}
	}
	if r := []rune(best); len(r) > 80 {
		best = strings.TrimSpace(string(r[:80]))
	}
	return best
}

// TicketBody is the description of a task created to fix an error group.
func TicketBody(project string, g ErrorGroup, filter, window string) string {
	return FlowTicketBody(project, g, filter, window, "")
}

// FlowTicketBody is TicketBody with the failing path the group belongs to
// (FlowSection) after the evidence.
func FlowTicketBody(project string, g ErrorGroup, filter, window, flowText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Lỗi production của service `%s` (dự án %s): %d lần trong %s", g.Service, project, g.Count, window)
	if !g.First.IsZero() {
		fmt.Fprintf(&b, ", lần đầu %s, gần nhất %s", g.First.Local().Format("02/01 15:04"), g.Last.Local().Format("02/01 15:04"))
	}
	b.WriteString(".\n\n")
	fmt.Fprintf(&b, "Thông điệp (đã chuẩn hoá): %s\n", g.Signature)
	if g.Sample != "" && g.Sample != g.Signature {
		fmt.Fprintf(&b, "Ví dụ thật: %s\n", clip(g.Sample, 500))
	}
	if g.Caller != "" {
		fmt.Fprintf(&b, "Caller: %s\n", g.Caller)
	}
	if g.Prefix != "" {
		fmt.Fprintf(&b, "Hàm (prefix): %s\n", g.Prefix)
	}
	if g.RequestID != "" {
		fmt.Fprintf(&b, "request_id gần nhất: %s\n", g.RequestID)
	}
	if st := strings.TrimSpace(g.Stacktrace); st != "" {
		fmt.Fprintf(&b, "\nStacktrace (rút gọn):\n%s\n", clip(st, 1500))
	}
	fmt.Fprintf(&b, "\nTìm lại trên Splunk (splunk_search):\n%s\n", Scope(filter, DrillSPL(g)))
	if extra := strings.TrimSpace(flowText); extra != "" {
		b.WriteString("\n" + extra + "\n")
	}
	b.WriteString("\nAcceptance criteria:\n")
	b.WriteString("- Xác định nguyên nhân gốc (đối chiếu log, code tại caller, dữ liệu QC nếu cần).\n")
	b.WriteString("- Sửa để lỗi không còn xuất hiện, hoặc nếu đây là lỗi dự kiến thì hạ mức log / trả lỗi rõ ràng hơn.\n")
	b.WriteString("- Có test tái hiện trường hợp gây lỗi.\n")
	return b.String()
}

// FlowSection describes the failing path of a group: its name and the other
// error groups seen along it (same requests/transactions, or the same call
// chain), so one task fixes the path instead of one log line.
func FlowSection(title string, related []ErrorGroup) string {
	var b strings.Builder
	b.WriteString("\n## Cùng luồng lỗi")
	if title != "" {
		b.WriteString(": " + title)
	}
	b.WriteString("\n")
	if len(related) == 0 {
		return b.String()
	}
	b.WriteString("Các nhóm lỗi khác trong cùng luồng (xử lý cùng nhau, tìm điểm gây lỗi đầu tiên trong chuỗi):\n")
	for _, r := range related {
		fmt.Fprintf(&b, "- `%s` %d lần: %s", r.Service, r.Count, clip(r.Signature, 200))
		if r.Caller != "" {
			fmt.Fprintf(&b, " (caller %s)", r.Caller)
		}
		if r.RequestID != "" {
			fmt.Fprintf(&b, " · request_id %s", r.RequestID)
		}
		b.WriteString("\n")
	}
	return b.String()
}
