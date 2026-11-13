package hasakiwork

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyEpic(t *testing.T) {
	for name, want := range map[string]EpicKind{
		"[EPIC] DEV Sprint 10":     EpicDev,
		"[EPIC] DEV & QC":          EpicDev,
		"[EPIC] DEVs & QC":         EpicDev,
		"[EPIC] Improve sprint 14": EpicImprove,
		"[EPIC] Issue Production":  EpicIssue,
		"[epic] issue production ": EpicIssue,
	} {
		if got, ok := ClassifyEpic(name); !ok || got != want {
			t.Errorf("%q → %q %v, want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"[EPIC] BA", "[EPIC] BOMv2", "[Issue Production] Fix RFID", "[BE] Improve query", "[EPIC] Developer portal"} {
		if k, ok := ClassifyEpic(name); ok {
			t.Errorf("%q must not be an EPIC kind, got %q", name, k)
		}
	}
}

func okJSON(data string) string {
	return `{"message":"Success","status":1,"code":200,"data":` + data + `}`
}

func TestCurrentEpics(t *testing.T) {
	details := map[string]string{
		// newest DEV EPIC is finished: the older open one wins.
		"300": `{"id":300,"code":"HSK-D300","name":"[EPIC] DEV Sprint 11","status":2,"prid":1,"milestone_id":31}`,
		"200": `{"id":200,"code":"HSK-D200","name":"[EPIC] DEV Sprint 10","status":1,"prid":1,"milestone_id":"21","milestone":{"id":21,"name":"Sprint 10: x"}}`,
		"210": `{"id":210,"code":"HSK-I210","name":"[EPIC] Improve Sprint 10","status":0,"prid":1,"milestone_id":null,"milestone":{"id":21,"name":"Sprint 10: x"}}`,
	}
	var lists []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/hr/projects/mytasks-es":
			q := r.URL.Query()
			lists = append(lists, q.Get("option")+" "+q.Get("prid")+" "+q.Get("status")+" "+q.Get("keyword"))
			if q.Get("keyword") == "EPIC" {
				io.WriteString(w, okJSON(`[{"id":300,"name":"[EPIC] DEV Sprint 11","prid":1},{"id":90,"name":"[EPIC] BA","prid":1}]`))
				return
			}
			// an open task under the Improve EPIC the search does not return
			io.WriteString(w, okJSON(`[{"id":5,"name":"[BE] x","prid":1,"parent":{"id":210,"name":"[EPIC] Improve Sprint 10","code":"HSK-I210"}}]`))
		case strings.HasPrefix(r.URL.Path, "/api/hr/projects/task-input/"):
			d, ok := details[strings.TrimPrefix(r.URL.Path, "/api/hr/projects/task-input/")]
			if !ok {
				w.WriteHeader(422)
				io.WriteString(w, `{"message":"You have no permision to access!"}`)
				return
			}
			io.WriteString(w, okJSON(d))
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	defer srv.Close()
	c := NewClient(staticToken("tok"))
	c.BaseURL = srv.URL + "/api"
	mine := []Task{
		{ID: 7, ProjectID: 1, Parent: &Ref{ID: 200, Name: "[EPIC] DEV Sprint 10"}},
		{ID: 8, ProjectID: 2, Parent: &Ref{ID: 999, Name: "[EPIC] Issue Production"}}, // other project
	}
	eps, err := c.CurrentEpics(context.Background(), 1, mine)
	if err != nil {
		t.Fatal(err)
	}
	if e := eps[EpicDev]; e == nil || e.ID != 200 || e.MilestoneID != 21 || e.Milestone != "Sprint 10: x" {
		t.Fatalf("dev = %+v", e)
	}
	if e := eps[EpicImprove]; e == nil || e.ID != 210 || e.MilestoneID != 21 {
		t.Fatalf("improve = %+v", e)
	}
	if e := eps[EpicIssue]; e != nil {
		t.Fatalf("issue must be missing (only another project's), got %+v", e)
	}
	if fmt.Sprint(lists) != "[all 1  EPIC all 1 _00_01 ]" {
		t.Fatalf("list calls = %q", lists)
	}
}

func TestAssignStaffAndProfile(t *testing.T) {
	var fields map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hr/projects/mass-update-field-task-input":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			fields = r.MultipartForm.Value
			io.WriteString(w, okJSON(`true`))
		case "/api/setting/user/profile":
			io.WriteString(w, okJSON(`{"profile":{"id":43149,"name":"N","email":"n@x"},"staff_info":{"staff_id":39324}}`))
		}
	}))
	defer srv.Close()
	c := NewClient(staticToken("tok"))
	c.BaseURL = srv.URL + "/api"
	if err := c.AssignStaff(context.Background(), 555, 39324); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(fields)
	if string(b) != `{"field":["assign_staff"],"id":["555"],"value[]":["39324"]}` {
		t.Fatalf("form = %s", b)
	}
	if err := c.AssignStaff(context.Background(), 555); err == nil {
		t.Error("no staff must fail")
	}
	p, err := c.Profile(context.Background())
	if err != nil || p.ID != 43149 || p.StaffID != 39324 {
		t.Fatalf("profile = %+v %v", p, err)
	}
}

func TestIsEpic(t *testing.T) {
	if !IsEpic("[EPIC] BA") || !IsEpic("[ Epic ] x") || IsEpic("[Issue Production] Fix RFID") || IsEpic("EPIC planning") {
		t.Fatal("IsEpic: only names tagged [EPIC]")
	}
}

func TestReportWritesResultThenStatus(t *testing.T) {
	var calls []string
	var result string
	var status map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hr/projects/mass-update-field-task-input":
			if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				calls = append(calls, fmt.Sprintf("field:%v=%v", b["field"], b["value"]))
				io.WriteString(w, okJSON(`true`))
				return
			}
			r.ParseMultipartForm(1 << 20)
			calls = append(calls, "result:"+r.FormValue("id")+":"+r.FormValue("field"))
			result = r.FormValue("value[configs][virtual_text]")
			io.WriteString(w, okJSON(`true`))
		case "/api/hr/projects/task-input/555":
			calls = append(calls, "detail")
			io.WriteString(w, okJSON(`{"id":555,"code":"HSK-X","staff":[{"id":901,"staff_id":11},{"id":902,"staff_id":39324}]}`))
		case "/api/hr/projects/update-assign-status":
			calls = append(calls, "status")
			json.NewDecoder(r.Body).Decode(&status)
			io.WriteString(w, okJSON(`true`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := NewClient(staticToken("tok"))
	c.BaseURL = srv.URL + "/api"
	ctx := context.Background()

	if err := c.Report(ctx, 555, 39324, "Đã chặn gen WO <SKU>\nPR #286\n\nCòn lại: không", 150, StatusWaitingApproval); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "result:555:data,field:reality_hours=150,detail,status" {
		t.Fatalf("calls = %v", calls)
	}
	if result != "<p>Đã chặn gen WO &lt;SKU&gt;<br>PR #286</p><p>Còn lại: không</p>" {
		t.Fatalf("result html = %q", result)
	}
	if fmt.Sprint(status) != "map[assign_id:902 status:5 task_id:555]" {
		t.Fatalf("status payload = %v", status)
	}
	// Not an assignee: the result is written, the status is not.
	calls = nil
	if err := c.Report(ctx, 555, 77, "x", 0, StatusFinished); err == nil || !strings.Contains(err.Error(), "không nằm trong danh sách người thực hiện") {
		t.Fatalf("not assigned: %v", err)
	}
	// Status 0: result only.
	calls = nil
	if err := c.Report(ctx, 555, 39324, "x", 0, 0); err != nil || strings.Join(calls, ",") != "result:555:data" {
		t.Fatalf("result only: %v %v", calls, err)
	}
}
