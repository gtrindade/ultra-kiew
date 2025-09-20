package mysql

import (
	"fmt"
)

type Equipment struct {
	ID                       int     `db:"id"`
	Name                     string  `db:"name"`
	Family                   *string `db:"family"`
	Category                 *string `db:"category"`
	Subcategory              *string `db:"subcategory"`
	Cost                     *string `db:"cost"`
	DmgS                     *string `db:"dmg_s"`
	ArmorShieldBonus         *string `db:"armor_shield_bonus"`
	MaximumDexBonus          *string `db:"maximum_dex_bonus"`
	DmgM                     *string `db:"dmg_m"`
	Weight                   *string `db:"weight"`
	Critical                 *string `db:"critical"`
	ArmorCheckPenalty        *string `db:"armor_check_penalty"`
	ArcaneSpellFailureChance *string `db:"arcane_spell_failure_chance"`
	RangeIncrement           *string `db:"range_increment"`
	Speed30                  *string `db:"speed_30"`
	Type                     *string `db:"type"`
	Speed20                  *string `db:"speed_20"`
	FullText                 *string `db:"full_text"`
	Reference                *string `db:"reference"`
}

func (c *Client) GetEquipmentByName(name string) ([]*Equipment, error) {
	var equipment []*Equipment

	rows, err := c.srd.Query(`
		SELECT 
			id,
			name,
			family,
			category,
			subcategory,
			cost,
			dmg_s,
			armor_shield_bonus,
			maximum_dex_bonus,
			dmg_m,
			weight,
			critical,
			armor_check_penalty,
			arcane_spell_failure_chance,
			range_increment,
			speed_30,
			type,
			speed_20,
			full_text,
			reference
		FROM equipment
		WHERE name LIKE ?
	`, "%"+name+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to query equipment: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var item Equipment
		if err := rows.Scan(
			&item.ID,
			&item.Name,
			&item.Family,
			&item.Category,
			&item.Subcategory,
			&item.Cost,
			&item.DmgS,
			&item.ArmorShieldBonus,
			&item.MaximumDexBonus,
			&item.DmgM,
			&item.Weight,
			&item.Critical,
			&item.ArmorCheckPenalty,
			&item.ArcaneSpellFailureChance,
			&item.RangeIncrement,
			&item.Speed30,
			&item.Type,
			&item.Speed20,
			&item.FullText,
			&item.Reference,
		); err != nil {
			return nil, fmt.Errorf("failed to scan equipment: %v", err)
		}
		equipment = append(equipment, &item)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over rows: %v", err)
	}

	if len(equipment) == 0 {
		return nil, nil
	}
	return equipment, nil
}
