package mysql

import (
	"fmt"
)

type Item struct {
	ID              int     `db:"id"`
	Name            string  `db:"name"`
	Category        *string `db:"category"`
	Subcategory     *string `db:"subcategory"`
	SpecialAbility  *string `db:"special_ability"`
	Aura            *string `db:"aura"`
	CasterLevel     *string `db:"caster_level"`
	Price           *string `db:"price"`
	ManifesterLevel *string `db:"manifester_level"`
	Prereq          *string `db:"prereq"`
	Cost            *string `db:"cost"`
	Weight          *string `db:"weight"`
	FullText        *string `db:"full_text"`
	Reference       *string `db:"reference"`
}

func (c *Client) GetItemsByName(name string) ([]*Item, error) {
	var items []*Item

	rows, err := c.srd.Query(`
		SELECT 
			id,
			name,
			category,
			subcategory,
			special_ability,
			aura,
			caster_level,
			price,
			manifester_level,
			prereq,
			cost,
			weight,
			full_text,
			reference
		FROM item
		WHERE name LIKE ?
	`, "%"+name+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to query items: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var item Item
		if err := rows.Scan(
			&item.ID,
			&item.Name,
			&item.Category,
			&item.Subcategory,
			&item.SpecialAbility,
			&item.Aura,
			&item.CasterLevel,
			&item.Price,
			&item.ManifesterLevel,
			&item.Prereq,
			&item.Cost,
			&item.Weight,
			&item.FullText,
			&item.Reference,
		); err != nil {
			return nil, fmt.Errorf("failed to scan item: %v", err)
		}
		items = append(items, &item)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over rows: %v", err)
	}

	if len(items) == 0 {
		return nil, nil
	}
	return items, nil
}
