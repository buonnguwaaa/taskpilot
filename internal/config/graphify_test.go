package config

import (
	"reflect"
	"testing"
)

func TestGraphLines(t *testing.T) {
	c := &Config{Graphify: &Graphify{Lines: map[string]string{"prod": "release/prod,$base", "qc": "release/qc"}},
		Projects: map[string]Project{
			"plm": {GraphifyLines: map[string]string{"staging": "staging"}, Repos: map[string]Repo{
				"fe":   {Lines: map[string]string{"qc": "qc"}},
				"base": {Lines: map[string]string{"qc": "-"}},
			}},
		}}
	if got := c.GraphLines("plm", "fe"); !reflect.DeepEqual(got, map[string]string{"prod": "release/prod,$base", "qc": "qc", "staging": "staging"}) {
		t.Fatalf("fe = %v", got)
	}
	if got := c.GraphLines("plm", "base"); got["qc"] != "" || got["prod"] == "" {
		t.Fatalf("base = %v", got)
	}
	if got := c.ProjectGraphLines("plm"); !reflect.DeepEqual(got, []string{"prod", "qc", "staging"}) {
		t.Fatalf("lines = %v", got)
	}
	c.Graphify = nil
	if len(c.GraphLines("plm", "fe")) != 0 {
		t.Fatal("off when not configured")
	}
}
