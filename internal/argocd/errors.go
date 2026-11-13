package argocd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Entry is one pod log line, read: zap JSON fields when the line is JSON,
// else the raw text.
type Entry struct {
	App        string
	At         time.Time
	Level      string // lower case: error, warn, info…; "" when unknown
	Message    string
	Caller     string
	Prefix     string
	RequestID  string
	Stacktrace string
	Raw        string
}

// ParseEntry reads a log line of app.
func ParseEntry(app string, l LogLine) Entry {
	e := Entry{App: app, Raw: strings.TrimSpace(l.Content)}
	if t, err := time.Parse(time.RFC3339Nano, l.Time); err == nil {
		e.At = t
	}
	var m map[string]any
	if strings.HasPrefix(e.Raw, "{") && json.Unmarshal([]byte(e.Raw), &m) == nil {
		s := func(k string) string {
			if v, ok := m[k]; ok && v != nil {
				if str, ok := v.(string); ok {
					return str
				}
				b, _ := json.Marshal(v)
				return string(b)
			}
			return ""
		}
		e.Level = strings.ToLower(s("level"))
		e.Message = s("message")
		if e.Message == "" {
			e.Message = s("msg")
		}
		if errText := s("error"); errText != "" && !strings.Contains(e.Message, errText) {
			e.Message = strings.TrimSpace(e.Message + ": " + errText)
		}
		e.Caller, e.Prefix, e.RequestID, e.Stacktrace = s("caller"), s("prefix"), s("request_id"), s("stacktrace")
		return e
	}
	e.Message = e.Raw
	switch lower := strings.ToLower(e.Raw); {
	case plainLevel.MatchString(lower):
		e.Level = plainLevel.FindStringSubmatch(lower)[1]
	case strings.Contains(lower, "panic:") || strings.HasPrefix(lower, "goroutine "):
		e.Level = "panic"
	}
	if e.Level == "warning" {
		e.Level = "warn"
	}
	return e
}

var plainLevel = regexp.MustCompile(`(?:^|[\s\[|])(error|fatal|panic|warn|warning|info|debug)(?:[\s\]|:]|$)`)

// IsError reports whether the entry is an error (or worse).
func (e Entry) IsError() bool {
	switch e.Level {
	case "error", "fatal", "panic", "dpanic":
		return true
	}
	return false
}

var (
	uuidRe  = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	longID  = regexp.MustCompile(`\b\w{24,}\b`)
	numbers = regexp.MustCompile(`\d+`)
	codeRe  = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_-]{7,}`)
)

// Signature masks what varies between occurrences of the same error (ids,
// numbers), like the grouping on the production logs.
func Signature(msg string) string {
	s := uuidRe.ReplaceAllString(msg, "<uuid>")
	s = longID.ReplaceAllString(s, "<id>")
	// Codes mixing letters and digits (HSK-T123-45KCVKFO6): one placeholder.
	s = codeRe.ReplaceAllStringFunc(s, func(tok string) string {
		if strings.ContainsAny(tok, "0123456789") && strings.IndexFunc(tok, func(r rune) bool {
			return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		}) >= 0 {
			return "<id>"
		}
		return tok
	})
	s = numbers.ReplaceAllString(s, "<n>")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// ErrorGroup is one kind of error of one app.
type ErrorGroup struct {
	App        string
	Signature  string
	Count      int
	First      time.Time
	Last       time.Time
	Sample     string // the latest real message
	Caller     string
	Prefix     string
	RequestID  string
	Stacktrace string
	Entries    []Entry // the latest occurrences (at most 20), oldest first
}

// GroupErrors groups the error entries by app and signature, most frequent
// first.
func GroupErrors(entries []Entry) []ErrorGroup {
	idx := map[string]int{}
	var out []ErrorGroup
	for _, e := range entries {
		if !e.IsError() {
			continue
		}
		sig := Signature(e.Message)
		if sig == "" {
			sig = "(không có message)"
		}
		k := e.App + "\x00" + sig
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, ErrorGroup{App: e.App, Signature: sig, First: e.At})
		}
		g := &out[i]
		g.Count++
		if !e.At.IsZero() && (g.First.IsZero() || e.At.Before(g.First)) {
			g.First = e.At
		}
		if e.At.After(g.Last) || g.Last.IsZero() {
			g.Last, g.Sample = e.At, e.Message
			for _, kv := range [][2]*string{{&g.Caller, &e.Caller}, {&g.Prefix, &e.Prefix}, {&g.RequestID, &e.RequestID}, {&g.Stacktrace, &e.Stacktrace}} {
				if *kv[1] != "" {
					*kv[0] = *kv[1]
				}
			}
		}
		g.Entries = append(g.Entries, e)
		if len(g.Entries) > 20 {
			g.Entries = g.Entries[len(g.Entries)-20:]
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// Format renders an entry on one line, in the same columns as the
// Production lines (splunk.Pretty): time, level, service, message, ids.
func (e Entry) Format() string {
	ts := "              "
	if !e.At.IsZero() {
		ts = e.At.Local().Format("01-02 15:04:05")
	}
	line := fmt.Sprintf("%s %-5s %-16s %s", ts, strings.ToUpper(e.Level), clipRunes(ShortApp(e.App), 16), e.Message)
	var extras []string
	if e.RequestID != "" {
		extras = append(extras, "request_id="+e.RequestID)
	}
	if e.Prefix != "" {
		extras = append(extras, "prefix="+e.Prefix)
	}
	if len(extras) > 0 {
		line += "  " + strings.Join(extras, " ")
	}
	return line
}

// ShortApp is the service an Argo CD app deploys: payment-qc-payment-vnpay →
// payment-vnpay, bom-api-qc → bom-api.
func ShortApp(app string) string {
	if i := strings.Index(app, "-qc-"); i >= 0 {
		return app[i+4:]
	}
	return strings.TrimSuffix(app, "-qc")
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
