package hasakiwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is the Hasaki Work backend.
const DefaultAPIURL = "https://wshr.hasaki.vn/api"

// Task statuses (STATUS_TASK_* in the web app).
const (
	StatusNew             = 0
	StatusProcessing      = 1
	StatusFinished        = 2
	StatusLate            = 3
	StatusCancelled       = 4
	StatusWaitingApproval = 5
	StatusFailed          = 6
)

// StatusName returns a readable name for a task status.
func StatusName(s int) string {
	switch s {
	case StatusNew:
		return "new"
	case StatusProcessing:
		return "processing"
	case StatusFinished:
		return "finished"
	case StatusLate:
		return "late"
	case StatusCancelled:
		return "cancelled"
	case StatusWaitingApproval:
		return "waiting_approval"
	case StatusFailed:
		return "failed"
	}
	return strconv.Itoa(s)
}

// TokenSource yields a bearer token (implemented by *Auth).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Client calls the Hasaki Work API.
type Client struct {
	Tokens  TokenSource
	BaseURL string
	Locale  string
	HTTP    *http.Client
}

// NewClient returns a client with defaults.
func NewClient(tokens TokenSource) *Client {
	return &Client{Tokens: tokens, BaseURL: DefaultAPIURL, Locale: "vi", HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// ErrUnauthorized means the API rejected the token (expired or blacklisted).
var ErrUnauthorized = errors.New("hasaki work rejected the token")

// envelope covers both response shapes the API uses:
// {"status":1,"code":200,"message":"Success","data":...} and {"status":200,"message":"OK","data":...}.
type envelope struct {
	Status  int             `json:"status"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string, out any) error {
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	u := strings.TrimRight(c.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Locale", c.Locale)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s %s: HTTP %d, non-JSON response", method, path, resp.StatusCode)
	}
	lower := strings.ToLower(env.Message)
	if resp.StatusCode == http.StatusUnauthorized || strings.Contains(lower, "blacklisted") ||
		strings.Contains(lower, "token") && (strings.Contains(lower, "expired") || strings.Contains(lower, "invalid")) {
		return fmt.Errorf("%w: %s", ErrUnauthorized, env.Message)
	}
	ok := resp.StatusCode < 300 && (env.Status == 1 || env.Status == 200 || env.Code == 200)
	if !ok {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, env.Message)
	}
	if out == nil {
		return nil
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("%s %s: empty data", method, path)
	}
	return json.Unmarshal(env.Data, out)
}

// Ref is a small {id, name, code} reference.
type Ref struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

// User is a person as embedded in tasks.
type User struct {
	UserID    int64  `json:"user_id"`
	StaffID   int64  `json:"staff_id"`
	StaffName string `json:"staff_name"`
	Email     string `json:"staff_email"`
	Title     string `json:"staff_title"`
}

// Task is a task as returned by the list and detail endpoints.
type Task struct {
	ID           int64   `json:"id"`
	Code         string  `json:"code"`
	Name         string  `json:"name"`
	Note         string  `json:"note"` // description (markdown); detail endpoint only
	Status       int     `json:"status"`
	Type         int     `json:"type"`
	ProjectID    int64   `json:"prid"`
	ParentID     int64   `json:"parent_id"`
	DateStart    string  `json:"date_start"`
	DateEnd      string  `json:"date_end"`
	PlannedHours float64 `json:"planned_hours"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	Tags         []any   `json:"tags"`
	Project      *Ref    `json:"project"`
	Parent       *Ref    `json:"parent"`
	Milestone    *Ref    `json:"milestone"`
	CreatedBy    *User   `json:"created_by_user"`
	AssignTo     []User  `json:"assign_to_user"`
}

// Profile is the logged-in user.
type Profile struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Profile returns the current user.
func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	var out struct {
		Profile Profile `json:"profile"`
	}
	err := c.do(ctx, http.MethodGet, "setting/user/profile", url.Values{"employee": {"1"}}, nil, "", &out)
	if err != nil {
		return nil, err
	}
	return &out.Profile, nil
}

// MyTasks lists tasks assigned to the current user. statusFilter is the web
// app's filter string: "_00_01" (new + processing), "_05", "_02", "_04".
func (c *Client) MyTasks(ctx context.Context, statusFilter string) ([]Task, error) {
	var all []Task
	const page = 100
	for offset := 0; ; offset += page {
		var batch []Task
		q := url.Values{
			"option":  {"my-task"},
			"status":  {statusFilter},
			"limit":   {strconv.Itoa(page)},
			"offset":  {strconv.Itoa(offset)},
			"to_date": {time.Now().Format("2006-01-02")},
		}
		if err := c.do(ctx, http.MethodGet, "hr/projects/mytasks-es", q, nil, "", &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < page {
			return all, nil
		}
	}
}

// Task returns one task with its description.
func (c *Client) Task(ctx context.Context, id int64) (*Task, error) {
	var t Task
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("hr/projects/task-input/%d", id), nil, nil, "", &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Comment is a task comment.
type Comment struct {
	ID        int64  `json:"id"`
	Comment   string `json:"comment"`
	CreatedAt string `json:"created_at"`
	UserID    int64  `json:"user_id"`
}

// Comments lists a task's comments.
func (c *Client) Comments(ctx context.Context, taskID int64) ([]Comment, error) {
	var raw json.RawMessage
	q := url.Values{"obj_id": {strconv.FormatInt(taskID, 10)}}
	if err := c.do(ctx, http.MethodGet, "v2/task/comment", q, nil, "", &raw); err != nil {
		if strings.HasSuffix(err.Error(), "empty data") {
			return nil, nil
		}
		return nil, err
	}
	// The endpoint returns either a bare array or {"rows": [...]}.
	var list []Comment
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Rows []Comment `json:"rows"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("comments: unexpected shape: %w", err)
	}
	return wrapped.Rows, nil
}

// AddComment posts a comment on a task (multipart, as the web app does).
// This WRITES to Hasaki Work; callers gate it behind configuration.
func (c *Client) AddComment(ctx context.Context, userID, taskID int64, text string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"user_id": strconv.FormatInt(userID, 10),
		"obj_id":  strconv.FormatInt(taskID, 10),
		"type":    "2",
		"comment": text,
	} {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "v2/task/comment", nil, &buf, w.FormDataContentType(), nil)
}
