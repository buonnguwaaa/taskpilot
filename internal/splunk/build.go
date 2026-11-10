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
	return base +
		` | eval service=coalesce('kubernetes.container_name', "?")` +
		` | eval m=coalesce(message, msg, error, _raw)` +
		` | eval sig=replace(m, "\w{8}-\w{4}-\w{4}-\w{4}-\w{12}", "<uuid>")` +
		` | eval sig=replace(sig, "0x\w+", "<hex>")` +
		` | eval sig=replace(sig, "\w*\d\w*\d\w*\d\w*", "<id>")` +
		` | eval sig=replace(sig, "\d+", "<n>")` +
		` | eval sig=substr(sig, 1, 160)` +
		` | stats count, min(_time) as first, max(_time) as last, latest(m) as sample, latest(caller) as caller,` +
		` latest(prefix) as prefix, latest(request_id) as request_id, latest(stacktrace) as stacktrace by service, sig` +
		` | sort - count | head ` + strconv.Itoa(limit)
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
	b.WriteString("\nAcceptance criteria:\n")
	b.WriteString("- Xác định nguyên nhân gốc (đối chiếu log, code tại caller, dữ liệu QC nếu cần).\n")
	b.WriteString("- Sửa để lỗi không còn xuất hiện, hoặc nếu đây là lỗi dự kiến thì hạ mức log / trả lỗi rõ ràng hơn.\n")
	b.WriteString("- Có test tái hiện trường hợp gây lỗi.\n")
	return b.String()
}
