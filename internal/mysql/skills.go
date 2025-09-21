package mysql

import (
	"fmt"
)

type Skill struct {
	ID          int     `db:"id"`
	Name        string  `db:"name"`
	Subtype     *string `db:"subtype"`
	KeyAbility  *string `db:"key_ability"`
	Psionic     *string `db:"psionic"`
	Trained     *string `db:"trained"`
	ArmorCheck  *string `db:"armor_check"`
	Description *string `db:"description"`
	SkillCheck  *string `db:"skill_check"`
	Action      *string `db:"action"`
	TryAgain    *string `db:"try_again"`
	Special     *string `db:"special"`
	Restriction *string `db:"restriction"`
	Synergy     *string `db:"synergy"`
	EpicUse     *string `db:"epic_use"`
	Untrained   *string `db:"untrained"`
	FullText    *string `db:"full_text"`
	Reference   *string `db:"reference"`
}

func (c *Client) GetSkillsByName(name string) ([]*Skill, error) {
	var skills []*Skill

	rows, err := c.srd.Query(`
		SELECT 
			id,
			name,
			subtype,
			key_ability,
			psionic,
			trained,
			armor_check,
			description,
			skill_check,
			action,
			try_again,
			special,
			restriction,
			synergy,
			epic_use,
			untrained,
			full_text,
			reference
		FROM skill
		WHERE name LIKE ?
	`, "%"+name+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to query skills: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var skill Skill
		if err := rows.Scan(
			&skill.ID,
			&skill.Name,
			&skill.Subtype,
			&skill.KeyAbility,
			&skill.Psionic,
			&skill.Trained,
			&skill.ArmorCheck,
			&skill.Description,
			&skill.SkillCheck,
			&skill.Action,
			&skill.TryAgain,
			&skill.Special,
			&skill.Restriction,
			&skill.Synergy,
			&skill.EpicUse,
			&skill.Untrained,
			&skill.FullText,
			&skill.Reference,
		); err != nil {
			return nil, fmt.Errorf("failed to scan skill: %v", err)
		}
		skills = append(skills, &skill)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over rows: %v", err)
	}

	if len(skills) == 0 {
		return nil, nil
	}
	return skills, nil
}
