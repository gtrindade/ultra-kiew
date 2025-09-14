package mysql

import (
	"fmt"
)

type Feat struct {
	ID              int    `db:"id"`
	RulebookID      int    `db:"rulebook_id"`
	Name            string `db:"name"`
	Description     string `db:"description"`
	Benefit         string `db:"benefit"`
	Special         string `db:"special"`
	Normal          string `db:"normal"`
	Page            *int16 `db:"page"`
	Slug            string `db:"slug"`
	DescriptionHTML string `db:"description_html"`
	BenefitHTML     string `db:"benefit_html"`
	SpecialHTML     string `db:"special_html"`
	NormalHTML      string `db:"normal_html"`

	// Additional fields for joined data
	Source     string `db:"source"`
	Categories string `db:"categories"`
}

func (c *Client) GetFeatByName(name string) ([]*Feat, error) {
	var feats []*Feat

	rows, err := c.db.Query(`
		SELECT 
			f.name,
			f.description,
			f.benefit,
			f.special,
			f.normal,
			r.name as source,
			GROUP_CONCAT(fc.name ORDER BY fc.name SEPARATOR ', ') as categories
		FROM 
			dnd_feat f
		JOIN 
			dnd_rulebook r ON f.rulebook_id = r.id
		LEFT JOIN
			dnd_feat_feat_categories ffc ON f.id = ffc.feat_id
		LEFT JOIN
			dnd_featcategory fc ON ffc.featcategory_id = fc.id
		WHERE 
			f.name LIKE ?
		GROUP BY 
			f.id, f.name, f.description, f.benefit, f.special, f.normal, r.name
	`, "%"+name+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to query feats: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var feat Feat
		if err := rows.Scan(
			&feat.Name,
			&feat.Description,
			&feat.Benefit,
			&feat.Special,
			&feat.Normal,
			&feat.Source,
			&feat.Categories,
		); err != nil {
			return nil, fmt.Errorf("failed to scan feat: %v", err)
		}
		feats = append(feats, &feat)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over rows: %v", err)
	}

	if len(feats) == 0 {
		return nil, nil // No feats found
	}
	return feats, nil
}
