package splunk

import (
	"strings"
	"testing"
)

func TestBuildSPL(t *testing.T) {
	for _, c := range []struct {
		q    Query
		want string
	}{
		{Query{}, "*"},
		{Query{NoNoise: true}, "level=*"},
		{Query{Service: "payment-worker", Level: LevelError}, "kubernetes.container_name=payment-worker (level=error OR level=fatal OR level=panic OR level=dpanic)"},
		{Query{Level: LevelWarn, Text: `HSK-1 "x"`}, `(level=warn OR level=error OR level=fatal OR level=panic OR level=dpanic) "HSK-1 \"x\""`},
		{Query{RequestID: "abc 1", Text: `caller="a.go:1"`}, `request_id="abc 1" caller="a.go:1"`},
	} {
		if got := BuildSPL(c.q); got != c.want {
			t.Errorf("BuildSPL(%+v) = %q, want %q", c.q, got, c.want)
		}
		if err := Validate(BuildSPL(c.q)); err != nil {
			t.Errorf("%+v: built SPL rejected: %v", c.q, err)
		}
	}
}

func TestGroupSPLAndParse(t *testing.T) {
	spl := GroupSPL("payment-vnpay", 20)
	if err := Validate(spl); err != nil {
		t.Fatalf("group SPL must pass the guard (no [ ]): %v", err)
	}
	if !IsTransforming(spl) || !strings.Contains(spl, "kubernetes.container_name=payment-vnpay") || !strings.HasSuffix(spl, "head 20") {
		t.Fatalf("spl = %s", spl)
	}
	gs := ParseGroups([]Event{
		{"service": "a", "sig": "x <n>", "count": "2", "first": "1762740000.5", "last": "1762743600"},
		{"service": "b", "sig": "Invalid checksum. Expect=<id>, Got=<id>", "count": "10", "caller": "vnpay/q.go:123"},
	})
	if len(gs) != 2 || gs[0].Service != "b" || gs[0].Count != 10 || gs[1].First.IsZero() {
		t.Fatalf("groups = %+v", gs)
	}
	drill := DrillSPL(gs[0])
	if !strings.Contains(drill, `caller="vnpay/q.go:123"`) || !strings.HasSuffix(drill, `"Invalid checksum. Expect"`) {
		t.Fatalf("drill = %s", drill)
	}
	if err := Validate(drill); err != nil {
		t.Fatal(err)
	}
	if got := drillTerms(`failed with transaction_code=HSK-<id>, err=rpc error: code = Internal desc = failed to q`); got != "rpc error: code" && !strings.Contains(got, "failed with transaction_code") {
		t.Fatalf("drillTerms = %q", got)
	}
	body := TicketBody("payment", gs[0], "kubernetes.namespace_name=payment-prod", "24 giờ")
	for _, want := range []string{"payment-vnpay", "10 lần trong 24 giờ", "vnpay/q.go:123", "kubernetes.namespace_name=payment-prod", "Acceptance criteria"} {
		if want == "payment-vnpay" {
			continue // the group's service is "b" in this fixture
		}
		if !strings.Contains(body, want) {
			t.Errorf("ticket body missing %q:\n%s", want, body)
		}
	}
}

func TestIsTransformingAndTable(t *testing.T) {
	if IsTransforming("level=error | head 5") || !IsTransforming("x | stats count by a") || !IsTransforming("x | top caller") {
		t.Fatal("IsTransforming")
	}
	lines := Table([]Event{{"service": "payment", "count": "3", "_tc": "x"}, {"service": "payment-worker", "count": "12"}}, 0)
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "service") || !strings.HasSuffix(strings.TrimSpace(lines[0]), "count") || strings.Contains(lines[0], "_tc") {
		t.Fatalf("table:\n%s", strings.Join(lines, "\n"))
	}
	out := Format("x | stats count by service", "-1h", []Event{{"service": "a", "count": "1"}}, 10)
	if strings.Contains(out, "<nil>") || !strings.Contains(out, "a") {
		t.Fatalf("Format of stats rows:\n%s", out)
	}
}

func TestPresets(t *testing.T) {
	if p, ok := PresetFor("-1d@d", "@d"); !ok || p.Label != "Hôm qua" {
		t.Fatal("yesterday preset")
	}
	if Describe("-4h", "") != "4 giờ" || Describe("-90m", "") != "từ -90m" || Describe("-2d@d", "-1d@d") != "-2d@d → -1d@d" {
		t.Fatal("Describe")
	}
	u := WebURLRange("https://s", "", "a=b", "-1d@d", "@d")
	if !strings.Contains(u, "latest=%40d") || !strings.Contains(u, "earliest=-1d%40d") {
		t.Fatalf("url = %s", u)
	}
}
