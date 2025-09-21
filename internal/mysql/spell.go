package mysql

import (
	"fmt"
	"time"
)

type Spell struct {
	ID                   int        `db:"id"`
	Added                time.Time  `db:"added"`
	RulebookID           int        `db:"rulebook_id"`
	Page                 *int16     `db:"page"`
	Name                 string     `db:"name"`
	SchoolID             int        `db:"school_id"`
	SubSchoolID          *int       `db:"sub_school_id"`
	VerbalComponent      bool       `db:"verbal_component"`
	SomaticComponent     bool       `db:"somatic_component"`
	MaterialComponent    bool       `db:"material_component"`
	ArcaneFocusComponent bool       `db:"arcane_focus_component"`
	DivineFocusComponent bool       `db:"divine_focus_component"`
	XPComponent          bool       `db:"xp_component"`
	CastingTime          *string    `db:"casting_time"`
	Range                *string    `db:"range"`
	Target               *string    `db:"target"`
	Effect               *string    `db:"effect"`
	Area                 *string    `db:"area"`
	Duration             *string    `db:"duration"`
	SavingThrow          *string    `db:"saving_throw"`
	SpellResistance      *string    `db:"spell_resistance"`
	Description          string     `db:"description"`
	Slug                 string     `db:"slug"`
	MetaBreathComponent  bool       `db:"meta_breath_component"`
	TrueNameComponent    bool       `db:"true_name_component"`
	ExtraComponents      *string    `db:"extra_components"`
	DescriptionHTML      string     `db:"description_html"`
	CorruptComponent     bool       `db:"corrupt_component"`
	CorruptLevel         *int16     `db:"corrupt_level"`
	Verified             bool       `db:"verified"`
	VerifiedAuthorID     *int       `db:"verified_author_id"`
	VerifiedTime         *time.Time `db:"verified_time"`

	// Additional fields for joined data
	Source      string `db:"source"`
	School      string `db:"school"`
	SubSchool   string `db:"sub_school"`
	ClassLevels string `db:"class_levels"`
	Components  string `db:"components"`
}

func (c *Client) GetSpellByName(name string) ([]*Spell, error) {
	var spells []*Spell

	rows, err := c.dndTools.Query(`
		SELECT 
				s.name,
				sc.name as school,
				COALESCE(sb.name, '') as sub_school,
				s.description,
				r.name as source,
				GROUP_CONCAT(CONCAT(c.name, ' ', scl.level) ORDER BY c.name SEPARATOR ', ') as class_levels,
				CONCAT_WS(', ', 
						IF(s.verbal_component, 'V', NULL),
						IF(s.somatic_component, 'S', NULL),
						IF(s.material_component, 'M', NULL),
						IF(s.arcane_focus_component, 'AF', NULL),
						IF(s.divine_focus_component, 'DF', NULL),
						IF(s.xp_component, 'XP', NULL),
						IF(s.meta_breath_component, 'MB', NULL),
						IF(s.true_name_component, 'TN', NULL),
						IF(s.corrupt_component, 'Corrupt', NULL)
				) as components,
				s.range,
				s.effect,
				s.area,
				s.duration,
				s.saving_throw,
				s.spell_resistance,
				s.extra_components
		FROM 
				dnd_spell s 
		JOIN 
				dnd_rulebook r ON s.rulebook_id = r.id
		JOIN
				dnd_spellschool sc ON s.school_id = sc.id
		LEFT JOIN
				dnd_spellsubschool sb ON s.sub_school_id = sb.id
		LEFT JOIN
				dnd_spellclasslevel scl ON s.id = scl.spell_id 
		LEFT JOIN
				dnd_characterclass c ON scl.character_class_id = c.id
		WHERE 
				s.name LIKE ?
		GROUP BY 
				s.id, s.name, sc.name, sb.name, s.description, r.name
`, "%"+name+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to query spells: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var spell Spell
		if err := rows.Scan(
			&spell.Name,
			&spell.School,
			&spell.SubSchool,
			&spell.Description,
			&spell.Source,
			&spell.ClassLevels,
			&spell.Components,
			&spell.Range,
			&spell.Effect,
			&spell.Area,
			&spell.Duration,
			&spell.SavingThrow,
			&spell.SpellResistance,
			&spell.ExtraComponents,
		); err != nil {
			return nil, fmt.Errorf("failed to scan spell: %v", err)
		}
		spells = append(spells, &spell)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over rows: %v", err)
	}

	if len(spells) == 0 {
		return nil, nil // No spells found
	}
	return spells, nil
}
