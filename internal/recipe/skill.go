package recipe

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// skillPrefix names the skill directories written for recipes, so a sync
// removes only those.
const skillPrefix = "recipe-"

// SyncSkills writes one skill per recipe into the plugin directory the
// agents load (<home>/plugin/skills/recipe-<name>/SKILL.md) and removes the
// skills of deleted recipes. Called when the plugin is written and after
// every change of a recipe; a running session sees the change on its next run.
func SyncSkills(home string) error {
	skills := filepath.Join(home, "plugin", "skills")
	if _, err := os.Stat(skills); err != nil {
		return nil // the plugin is not written yet: Materialize calls again
	}
	old, _ := filepath.Glob(filepath.Join(skills, skillPrefix+"*"))
	for _, d := range old {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
	}
	rs, _ := List(Dir(home), "")
	for _, r := range rs {
		dir := filepath.Join(skills, skillPrefix+r.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(Skill(r)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Skill is the SKILL.md of a recipe.
func Skill(r *Recipe) string {
	var b strings.Builder
	desc := oneLine(r.Description)
	if desc != "" && !strings.ContainsAny(desc[len(desc)-1:], ".!?") {
		desc += "."
	}
	fmt.Fprintf(&b, "---\nname: %s%s\ndescription: %s Operator-made debugging recipe of project %s: run it with the taskpilot tool run_recipe instead of writing these lookups yourself.\n---\n\n",
		skillPrefix, r.Name, strings.ReplaceAll(desc, "\n", " "), r.Project)
	fmt.Fprintf(&b, "# Recipe %s (project %s)\n\n%s\n\n", r.Name, r.Project, strings.TrimSpace(r.Description))
	b.WriteString("Only for tasks of project " + r.Project + ". Run it in one call, with every parameter:\n\n")
	b.WriteString("```\nrun_recipe {\"name\": \"" + r.Name + "\", \"params\": {")
	for i, p := range r.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		ex := p.Example
		if ex == "" {
			ex = "…"
		}
		fmt.Fprintf(&b, "%q: %q", p.Name, ex)
	}
	b.WriteString("}}\n```\n\n")
	if len(r.Params) > 0 {
		b.WriteString("Parameters:\n")
		for _, p := range r.Params {
			fmt.Fprintf(&b, "- `%s`: %s\n", p.Name, p.Desc)
		}
		b.WriteString("\n")
	}
	b.WriteString("It runs these read-only lookups in order (each with the checks of the matching tool) and returns all results:\n")
	for i, s := range r.Steps {
		title := s.Title
		if title == "" {
			title = s.Kind()
		}
		fmt.Fprintf(&b, "%d. %s: `%s`\n", i+1, title, s.Command())
	}
	if g := strings.TrimSpace(r.Guide); g != "" {
		b.WriteString("\n## Reading the results\n\n" + g + "\n")
	}
	b.WriteString("\nThe recipe is a starting point: continue with your own lookups when its results do not answer the question.\n")
	return b.String()
}
