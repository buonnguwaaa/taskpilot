package hasakiwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is the Hasaki Work backend.
const DefaultAPIURL = "https://wshr.hasaki.vn/api"

// DefaultMediaURL serves the files of tasks (note_files are paths under it).
// The web app builds it as getMediaUrl() + "/production/hr/"; the files are
// public, no token is sent.
const DefaultMediaURL = "https://hr-media.hasaki.vn/production/hr/"

// MaxFileSize caps one downloaded task file.
const MaxFileSize = 100 << 20

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
	// MediaURL is the base of task file paths (DefaultMediaURL).
	MediaURL string
	Locale   string
	HTTP     *http.Client
}

// NewClient returns a client with defaults.
func NewClient(tokens TokenSource) *Client {
	return &Client{Tokens: tokens, BaseURL: DefaultAPIURL, MediaURL: DefaultMediaURL, Locale: "vi", HTTP: &http.Client{Timeout: 60 * time.Second}}
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

// GetRaw GETs path and returns the data of the response, for exploring the API.
func (c *Client) GetRaw(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, path, query, nil, "", &out)
	return out, err
}

// FlexInt is an integer the API sends as a number, a string or null.
type FlexInt int64

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not an integer: %s", b)
	}
	*f = FlexInt(n)
	return nil
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
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	Note string `json:"note"` // description (markdown); detail endpoint only
	// NoteFiles are the files attached to the description, as paths under
	// the media URL (detail endpoint only).
	NoteFiles    []string `json:"note_files"`
	Status       int      `json:"status"`
	Type         int      `json:"type"`
	ProjectID    int64    `json:"prid"`
	ParentID     int64    `json:"parent_id"`
	MilestoneID  FlexInt  `json:"milestone_id"`
	DateStart    string   `json:"date_start"`
	DateEnd      string   `json:"date_end"`
	PlannedHours float64  `json:"planned_hours"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Tags         []any    `json:"tags"`
	Project      *Ref     `json:"project"`
	Parent       *Ref     `json:"parent"`
	Milestone    *Ref     `json:"milestone"`
	CreatedBy    *User    `json:"created_by_user"`
	AssignTo     []User   `json:"assign_to_user"`
	// Staff are the assignment rows (one per assignee): their id is the
	// assign_id of update-assign-status.
	Staff []Assignment `json:"staff"`
}

// Assignment is one assignee of a task.
type Assignment struct {
	ID      int64 `json:"id"`
	StaffID int64 `json:"staff_id"`
	Status  int   `json:"status"`
}

// Profile is the logged-in user.
type Profile struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	// StaffID identifies the user as an assignee (staff_info.staff_id).
	StaffID int64 `json:"-"`
}

// Profile returns the current user.
func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	var out struct {
		Profile   Profile `json:"profile"`
		StaffInfo struct {
			StaffID int64 `json:"staff_id"`
		} `json:"staff_info"`
	}
	err := c.do(ctx, http.MethodGet, "setting/user/profile", url.Values{"employee": {"1"}}, nil, "", &out)
	if err != nil {
		return nil, err
	}
	out.Profile.StaffID = out.StaffInfo.StaffID
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

// FileURL is where a task file path (NoteFiles) is served.
func (c *Client) FileURL(path string) string {
	base := c.MediaURL
	if base == "" {
		base = DefaultMediaURL
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// FileName is the local name of a task file: its base name without the
// upload stamp the server adds ("spec_2026-11-10_103428.pdf" → "spec.pdf").
func FileName(path string) string {
	base := filepath.Base(filepath.FromSlash(strings.ReplaceAll(path, `\`, "/")))
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "file"
	}
	ext := filepath.Ext(base)
	stem := uploadStamp.ReplaceAllString(strings.TrimSuffix(base, ext), "")
	if stem == "" {
		stem = "file"
	}
	return stem + ext
}

var uploadStamp = regexp.MustCompile(`_\d{4}-\d{2}-\d{2}_\d{6}$`)

// DownloadFile saves a task file (a NoteFiles path) into dir and returns the
// local path. A name already taken by another file gets a -2, -3… suffix.
func (c *Client) DownloadFile(ctx context.Context, path, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.FileURL(path), nil)
	if err != nil {
		return "", err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	if resp.ContentLength > MaxFileSize {
		return "", fmt.Errorf("%s: too large (%d MB)", path, resp.ContentLength>>20)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := FileName(path)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	var f *os.File
	for i := 1; ; i++ {
		dst := filepath.Join(dir, name)
		if i > 1 {
			dst = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		}
		f, err = os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, MaxFileSize+1))
	if err == nil && n > MaxFileSize {
		err = fmt.Errorf("%s: too large (over %d MB)", path, MaxFileSize>>20)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
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

// NewTask is the payload of upsert-task-input for a new task, as the web app
// sends it. Assignees are set afterwards (AssignStaff).
type NewTask struct {
	Name        string
	Note        string // description (markdown); empty: none
	ProjectID   int64  // prid
	ParentID    int64  // usually the EPIC; 0: none
	MilestoneID int64  // sprint; 0: none
	// PlannedMinutes goes to planned_hours, which the web app fills in minutes (480 = 8h).
	PlannedMinutes int
	DateEnd        time.Time // zero: none
}

// Created identifies a task just created.
type Created struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

// CreateTask creates a task, unassigned as the web app's create dialog does
// (no_assign=1; see AssignStaff). This WRITES to Hasaki Work.
func (c *Client) CreateTask(ctx context.Context, t NewTask) (*Created, error) {
	if strings.TrimSpace(t.Name) == "" || t.ProjectID == 0 {
		return nil, errors.New("create task: name and project are required")
	}
	p := map[string]any{
		"id":            0,
		"name":          strings.TrimSpace(t.Name),
		"prid":          t.ProjectID,
		"no_assign":     1,
		"sub_type":      0,
		"planned_hours": t.PlannedMinutes,
	}
	if t.ParentID != 0 {
		p["parent_id"] = t.ParentID
	}
	if t.MilestoneID != 0 {
		p["milestone_id"] = t.MilestoneID
	}
	if n := strings.TrimSpace(t.Note); n != "" {
		p["note"] = n
	}
	if !t.DateEnd.IsZero() {
		p["date_end"] = t.DateEnd.Format("2006-01-02") + " 00:00:00"
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	// data is the new task; only id and code are read (planned_hours comes back as a string).
	var out Created
	if err := c.do(ctx, http.MethodPost, "hr/projects/upsert-task-input/0", nil, bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	if out.ID == 0 || out.Code == "" {
		return nil, fmt.Errorf("create task: response has no id/code (id=%d code=%q)", out.ID, out.Code)
	}
	return &out, nil
}

// resultHTML turns plain text (paragraphs separated by blank lines) into the
// HTML the web app's result editor stores.
func resultHTML(text string) string {
	var paras []string
	for _, p := range strings.Split(strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n"), "\n\n") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		lines := strings.Split(p, "\n")
		for i, l := range lines {
			lines[i] = html.EscapeString(l)
		}
		paras = append(paras, "<p>"+strings.Join(lines, "<br>")+"</p>")
	}
	return strings.Join(paras, "")
}

// UpdateResult writes the task's work result ("Kết quả công việc"), as the
// web app's result editor does. This WRITES to Hasaki Work.
func (c *Client) UpdateResult(ctx context.Context, taskID int64, text string) error {
	if taskID == 0 || strings.TrimSpace(text) == "" {
		return errors.New("update result: task and text are required")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("id", strconv.FormatInt(taskID, 10))
	w.WriteField("field", "data")
	w.WriteField("value[configs][virtual_text]", resultHTML(text))
	if err := w.Close(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "hr/projects/mass-update-field-task-input", nil, &buf, w.FormDataContentType(), nil)
}

// SetAssignStatus sets the status of one assignee of a task (2 done, 5
// waiting for approval…), as the web app's status menu does. Hasaki Work
// refuses "done" while the task has no work result. This WRITES.
func (c *Client) SetAssignStatus(ctx context.Context, taskID, assignID int64, status int) error {
	body, err := json.Marshal(map[string]any{"task_id": taskID, "assign_id": assignID, "status": status})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "hr/projects/update-assign-status", nil, bytes.NewReader(body), "application/json", nil)
}

// SetRealityMinutes writes the actual time spent on the task ("Giờ thực
// tế"), in minutes like planned_hours, as the web app's task table does.
// This WRITES to Hasaki Work.
func (c *Client) SetRealityMinutes(ctx context.Context, taskID int64, minutes int) error {
	if taskID == 0 || minutes <= 0 {
		return errors.New("reality hours: task and a positive time are required")
	}
	body, err := json.Marshal(map[string]any{"id": taskID, "field": "reality_hours", "value": minutes})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "hr/projects/mass-update-field-task-input", nil, bytes.NewReader(body), "application/json", nil)
}

// Report writes the work result and the actual time (realityMinutes, 0 =
// unchanged), then sets the status of the operator's assignment (staffID;
// status 0 = unchanged).
func (c *Client) Report(ctx context.Context, taskID, staffID int64, result string, realityMinutes, status int) error {
	if err := c.UpdateResult(ctx, taskID, result); err != nil {
		return fmt.Errorf("ghi kết quả công việc: %w", err)
	}
	if realityMinutes > 0 {
		if err := c.SetRealityMinutes(ctx, taskID, realityMinutes); err != nil {
			return fmt.Errorf("ghi giờ thực tế: %w", err)
		}
	}
	if status == 0 {
		return nil
	}
	t, err := c.Task(ctx, taskID)
	if err != nil {
		return fmt.Errorf("đọc task để đổi trạng thái: %w", err)
	}
	var assign int64
	for _, a := range t.Staff {
		if a.StaffID == staffID {
			assign = a.ID
		}
	}
	if assign == 0 {
		return fmt.Errorf("bạn không nằm trong danh sách người thực hiện %s nên không đổi được trạng thái", t.Code)
	}
	if err := c.SetAssignStatus(ctx, taskID, assign, status); err != nil {
		return fmt.Errorf("đổi trạng thái: %w", err)
	}
	return nil
}
