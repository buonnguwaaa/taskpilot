package runner

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Quota signals Claude Code (or similar) usage/rate-limit exhaustion.
type Quota struct {
	Hit     bool
	Reason  string
	Resets  time.Time // zero if unknown
	Source  string    // result | rate_limit_event | stderr | exit
}

var quotaText = regexp.MustCompile(`(?i)(you've hit your (weekly|session|usage|rate)?\s*limit|hit your limit|rate.?limit|usage.?limit|quota|out of (extra )?usage|out_of_credits|credits?_required|billing_error|credit balance|too many requests|429)`)

var resetsAtText = regexp.MustCompile(`(?i)resets?\s+([A-Za-z]{3}\s+\d{1,2},\s+\d{1,2}(?::\d{2})?\s*(?:am|pm)?(?:\s*\([^)]+\))?)`)

// ParseQuotaReason reports whether text describes a quota/rate-limit failure.
func ParseQuotaReason(text string) (reason string, ok bool) {
	text = strings.TrimSpace(text)
	if text == "" || !quotaText.MatchString(text) {
		return "", false
	}
	return clip(text, 300), true
}

func (q *Quota) note(reason, source string) {
	if reason == "" {
		return
	}
	q.Hit = true
	if q.Reason == "" {
		q.Reason = reason
	}
	if q.Source == "" {
		q.Source = source
	}
}

func (q *Quota) noteResetsUnix(sec int64) {
	if sec <= 0 {
		return
	}
	t := time.Unix(sec, 0).UTC()
	if q.Resets.IsZero() || t.After(q.Resets) {
		q.Resets = t
	}
}

func observeQuotaEvent(line []byte, q *Quota) {
	var typed struct {
		Type   string `json:"type"`
		Subtype string `json:"subtype"`
		Error  string `json:"error"`
		IsError bool  `json:"is_error"`
		Result string `json:"result"`
		APIErrorStatus int `json:"api_error_status"`
		APIErrorCode string `json:"api_error_code"`
		APIError string `json:"api_error"`
		RateLimitInfo json.RawMessage `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &typed) != nil {
		return
	}
	switch typed.Type {
	case "rate_limit_event":
		var info struct {
			Status               string `json:"status"`
			ResetsAt             int64  `json:"resetsAt"`
			OverageStatus        string `json:"overageStatus"`
			OverageDisabledReason string `json:"overageDisabledReason"`
			IsUsingOverage       bool   `json:"isUsingOverage"`
			ErrorCode            string `json:"errorCode"`
		}
		_ = json.Unmarshal(typed.RateLimitInfo, &info)
		if info.Status == "rejected" && !info.IsUsingOverage {
			reason := "Claude rate limit rejected"
			if info.OverageDisabledReason != "" {
				reason += " (" + info.OverageDisabledReason + ")"
			} else if info.ErrorCode != "" {
				reason += " (" + info.ErrorCode + ")"
			}
			q.note(reason, "rate_limit_event")
			q.noteResetsUnix(info.ResetsAt)
		}
	case "system":
		if typed.Subtype == "api_retry" && (typed.Error == "rate_limit" || typed.Error == "billing_error") {
			q.note("Claude API "+typed.Error, "api_retry")
		}
	case "result":
		if typed.APIErrorStatus == 429 || strings.Contains(strings.ToLower(typed.APIErrorCode), "credit") ||
			strings.Contains(strings.ToLower(typed.APIError), "credit") || strings.Contains(strings.ToLower(typed.APIError), "rate_limit") {
			reason := typed.Result
			if reason == "" {
				reason = "Claude API status " + strconv.Itoa(typed.APIErrorStatus)
			}
			q.note(clip(reason, 300), "result")
		}
		if typed.IsError {
			if reason, ok := ParseQuotaReason(typed.Result); ok {
				q.note(reason, "result")
			}
			if reason, ok := ParseQuotaReason(typed.Subtype); ok {
				q.note(reason, "result")
			}
		} else if reason, ok := ParseQuotaReason(typed.Result); ok {
			// Seen in the wild: is_error true with subtype success; also guard text-only.
			q.note(reason, "result")
		}
	}
}

func observeQuotaText(text, source string, q *Quota) {
	if reason, ok := ParseQuotaReason(text); ok {
		q.note(reason, source)
	}
}

// DefaultQuotaLatch how long to prefer Cursor when Claude is out and no resetsAt is known.
const DefaultQuotaLatch = time.Hour
