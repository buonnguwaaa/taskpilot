package plugin

import (
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// The subagents the workflow skill names must ship, with the tools they need
// and none of the task lifecycle tools.
func TestSubagents(t *testing.T) {
	for name, model := range map[string]string{"explorer": "sonnet", "reviewer": "opus", "implementer": ""} {
		b, err := Read("agents/" + name + ".md")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := string(b)
		if !strings.HasPrefix(s, "---\nname: "+name+"\n") {
			t.Errorf("%s: bad frontmatter", name)
		}
		if model != "" && !strings.Contains(s, "\nmodel: "+model+"\n") {
			t.Errorf("%s: model %s expected", name, model)
		}
		for _, tool := range []string{"request_approval", "complete_task", "create_pull_request", "set_execution_profile", "Agent"} {
			if strings.Contains(strings.SplitN(s, "\n---", 2)[0], tool) {
				t.Errorf("%s must not get %s", name, tool)
			}
		}
	}
	skill, _ := Read("skills/workflow/SKILL.md")
	for _, want := range []string{"set_execution_profile", "taskpilot:explorer", "taskpilot:reviewer", "taskpilot:implementer", "Do not spawn when"} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("workflow skill must mention %q", want)
		}
	}
}

// Investigation tickets are recognised by the marker their body starts with.
func TestSkillKnowsInvestigationTickets(t *testing.T) {
	skill, _ := Read("skills/workflow/SKILL.md")
	if !strings.Contains(string(skill), "`"+splunk.QCInvestigationMarker+"`") {
		t.Fatalf("the workflow skill must name the marker %q", splunk.QCInvestigationMarker)
	}
	if !strings.Contains(string(skill), "`"+splunk.InvestigationMarker+"`") {
		t.Fatalf("the workflow skill must name the marker %q", splunk.InvestigationMarker)
	}
}
