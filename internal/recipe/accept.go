package recipe

import (
	"fmt"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// Accept saves a waiting proposal as a recipe (replacing the one it
// improves) and refreshes the agents' skills.
func Accept(st *store.Store, id int64, known func(string) bool) (*Recipe, error) {
	p, err := st.RecipeProposal(id)
	if err != nil {
		return nil, err
	}
	if p.Status != "new" {
		return nil, fmt.Errorf("đề xuất #%d đã %s", id, map[string]string{"accepted": "được chấp nhận", "rejected": "bị bỏ"}[p.Status])
	}
	r, err := Parse([]byte(p.YAML))
	if err != nil {
		return nil, err
	}
	r.Project = p.Project
	dir := Dir(st.Home)
	if err := Save(dir, r, known); err != nil {
		return nil, err
	}
	if p.Replaces != "" && p.Replaces != r.Name {
		_ = Remove(dir, p.Replaces)
	}
	if err := st.DecideRecipeProposal(id, "accepted"); err != nil {
		return nil, err
	}
	return r, SyncSkills(st.Home)
}

// Reject marks a waiting proposal rejected: reflect does not propose it again.
func Reject(st *store.Store, id int64) error {
	p, err := st.RecipeProposal(id)
	if err != nil {
		return err
	}
	if p.Status != "new" {
		return fmt.Errorf("đề xuất #%d không còn chờ duyệt", id)
	}
	return st.DecideRecipeProposal(id, "rejected")
}

// EditProposal replaces a waiting proposal's recipe with the operator's
// version, once valid.
func EditProposal(st *store.Store, id int64, yamlText string, known func(string) bool) error {
	p, err := st.RecipeProposal(id)
	if err != nil {
		return err
	}
	r, err := Parse([]byte(yamlText))
	if err != nil {
		return err
	}
	r.Project = p.Project
	if err := Validate(r, known); err != nil {
		return err
	}
	return st.EditRecipeProposal(id, r.Name, Marshal(r))
}
