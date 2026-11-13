package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSplunkLimitsZeroMeansUnlimited(t *testing.T) {
	dir := t.TempDir()
	load := func(extra string) *Config {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte("home: "+dir+"\nsplunk:\n  url: https://s\n"+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := load(""); c.Splunk.MaxLookback != 7*24*time.Hour || c.Splunk.MaxEvents != 100 {
		t.Fatalf("defaults = %v %d", c.Splunk.MaxLookback, c.Splunk.MaxEvents)
	}
	if c := load("  max_lookback: 0s\n  max_events: 0\n"); c.Splunk.MaxLookback != 0 || c.Splunk.MaxEvents != 0 {
		t.Fatalf("explicit 0 = %v %d", c.Splunk.MaxLookback, c.Splunk.MaxEvents)
	}
}

func TestDefaultBranchRule(t *testing.T) {
	re := Project{}.BranchRule()
	for name, want := range map[string]bool{
		"feat/add-hawaiian-lifestyle-category--PLM_DSN_STY_0013": true,
		"fix/update-step--BOM_CUS_INF_0003":                      true,
		"feat/x--WHR_DOC_CAT_0001,WHR_DOC_CAT_0002":              true,
		"hotfix/order-item--HSK-SLJ68DMP":                        true,
		"tp/hw-HSK-SLJ68DMP":                                     false,
		"feat/add-hawaiian-lifestyle-category":                   false,
		"feat/Add_Category--PLM_DSN_STY_0013":                    false,
		"feature/x--PLM_DSN_STY_0013":                            false,
	} {
		if re.MatchString(name) != want {
			t.Errorf("%s: want %v", name, want)
		}
	}
}

func TestAgentMayRenameBranchesWithGit(t *testing.T) {
	for _, cmd := range []string{"git -C /w/api branch -m tp/x feat/y--A_B_0001", "git checkout -b feat/x", "git switch -c feat/x"} {
		for _, r := range DefaultRules() {
			if r.Deny != "" && r.Command != "" && regexp.MustCompile(r.Command).MatchString(cmd) {
				t.Errorf("%q denied: %s", cmd, r.Deny)
			}
		}
	}
}

func TestPushRulesSeeGitGlobalOptions(t *testing.T) {
	var gate, force, cfg *regexp.Regexp
	for _, r := range DefaultRules() {
		switch {
		case r.Requires == "pr" && strings.Contains(r.Command, "push"):
			gate = regexp.MustCompile(r.Command)
		case strings.Contains(r.Deny, "force-push"):
			force = regexp.MustCompile(r.Command)
		case strings.Contains(r.Deny, "git -c"):
			cfg = regexp.MustCompile(r.Command)
		}
	}
	for cmd, want := range map[string][3]bool{ // gate, force, cfg
		"git push origin HEAD":                     {true, false, false},
		"git -C /w/api push origin HEAD":           {true, false, false},
		"cd x && git -C api push --force origin y": {true, true, false},
		"git push origin +HEAD:feat/x":             {true, true, false},
		"git -C api push -f":                       {true, true, false},
		"git -C api log --oneline":                 {false, false, false},
		"git commit -m 'fix -f flag'":              {false, false, false},
		"git -C . -c alias.x='!sh -c id' x":        {false, false, true},
		"git -c core.sshCommand=evil fetch":        {false, false, true},
		"git -c user.name=bot commit -m x":         {false, false, false},
	} {
		got := [3]bool{gate.MatchString(cmd), force.MatchString(cmd), cfg.MatchString(cmd)}
		if got != want {
			t.Errorf("%q: gate/force/cfg = %v, want %v", cmd, got, want)
		}
	}
}
