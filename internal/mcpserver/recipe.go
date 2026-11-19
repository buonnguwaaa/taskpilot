package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/recipe"
)

// RunRecipeIn is the input of run_recipe.
type RunRecipeIn struct {
	Name   string            `json:"name,omitempty" jsonschema:"the recipe (skill recipe-<name>); empty lists the project's recipes"`
	Params map[string]string `json:"params,omitempty" jsonschema:"every parameter of the recipe, e.g. {\"code\": \"261116VN91SQ\"}"`
}

// RunRecipe runs one of the project's recipes with the checks of the agent
// tools (and their audit, inside a task).
func (h *Handlers) RunRecipe(ctx context.Context, in RunRecipeIn) (string, error) {
	project, err := h.project()
	if err != nil {
		return "", err
	}
	dir := recipe.Dir(h.Store.Home)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		rs, _ := recipe.List(dir, project)
		if len(rs) == 0 {
			return "Project " + project + " has no recipe yet.", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Recipes of %s:\n", project)
		for _, r := range rs {
			var ps []string
			for _, p := range r.Params {
				ps = append(ps, p.Name)
			}
			fmt.Fprintf(&b, "- %s(%s): %s\n", r.Name, strings.Join(ps, ", "), oneLine(r.Description))
		}
		return b.String(), nil
	}
	r, err := recipe.Load(dir, name)
	if err != nil {
		return "", err
	}
	if r.Project != project {
		return "", fmt.Errorf("recipe %s belongs to project %s, not %s", r.Name, r.Project, project)
	}
	if err := recipe.Validate(r, nil); err != nil {
		return "", fmt.Errorf("recipe %s is invalid: %w", r.Name, err)
	}
	res, err := recipe.Run(ctx, r, in.Params, RecipeExec{h})
	if err != nil {
		return "", err
	}
	return recipe.Format(r, in.Params, res), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// RecipeExec runs recipe steps through the agent tools of h.
type RecipeExec struct{ H *Handlers }

func (x RecipeExec) SQL(ctx context.Context, s recipe.SQLStep) (string, error) {
	return x.H.DBQuery(ctx, DBQueryIn{SQL: s.Query, MaxRows: s.MaxRows})
}

func (x RecipeExec) Splunk(ctx context.Context, s recipe.SplunkStep) (string, error) {
	return x.H.SplunkSearch(ctx, SplunkSearchIn{Query: s.Query, Earliest: s.Earliest, Latest: s.Latest, MaxEvents: s.MaxEvents})
}

func (x RecipeExec) QCLogs(ctx context.Context, s recipe.QCLogsStep) (string, error) {
	return x.H.QCLogs(ctx, QCLogsIn{App: s.App, Filter: s.Filter, Tail: s.Tail, SinceMinutes: s.SinceMinutes})
}

func (x RecipeExec) Vault(ctx context.Context, s recipe.VaultStep) (string, error) {
	return x.H.QCConfigGet(ctx, QCConfigIn{Path: s.Path})
}

func (x RecipeExec) Kafka(ctx context.Context, s recipe.KafkaStep) (string, error) {
	return x.H.QCKafkaMessages(ctx, QCKafkaMessagesIn{Topic: s.Topic, Filter: s.Filter, Since: s.Since, Limit: s.Limit})
}
