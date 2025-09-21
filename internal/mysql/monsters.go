package mysql

import (
	"fmt"
)

type Monster struct {
	ID               int     `db:"id"`
	Family           *string `db:"family"`
	Name             string  `db:"name"`
	Altname          *string `db:"altname"`
	Size             *string `db:"size"`
	Type             *string `db:"type"`
	Descriptor       *string `db:"descriptor"`
	HitDice          *string `db:"hit_dice"`
	Initiative       *string `db:"initiative"`
	Speed            *string `db:"speed"`
	ArmorClass       *string `db:"armor_class"`
	BaseAttack       *string `db:"base_attack"`
	Grapple          *string `db:"grapple"`
	Attack           *string `db:"attack"`
	FullAttack       *string `db:"full_attack"`
	Space            *string `db:"space"`
	Reach            *string `db:"reach"`
	SpecialAttacks   *string `db:"special_attacks"`
	SpecialQualities *string `db:"special_qualities"`
	Saves            *string `db:"saves"`
	Abilities        *string `db:"abilities"`
	Skills           *string `db:"skills"`
	BonusFeats       *string `db:"bonus_feats"`
	Feats            *string `db:"feats"`
	EpicFeats        *string `db:"epic_feats"`
	Environment      *string `db:"environment"`
	Organization     *string `db:"organization"`
	ChallengeRating  *string `db:"challenge_rating"`
	Treasure         *string `db:"treasure"`
	Alignment        *string `db:"alignment"`
	Advancement      *string `db:"advancement"`
	LevelAdjustment  *string `db:"level_adjustment"`
	SpecialAbilities *string `db:"special_abilities"`
	StatBlock        *string `db:"stat_block"`
	FullText         *string `db:"full_text"`
	Reference        *string `db:"reference"`
}

func (c *Client) GetMonstersByName(name string) ([]*Monster, error) {
	var monsters []*Monster

	rows, err := c.srd.Query(`
		SELECT 
			id,
			family,
			name,
			altname,
			size,
			type,
			descriptor,
			hit_dice,
			initiative,
			speed,
			armor_class,
			base_attack,
			grapple,
			attack,
			full_attack,
			space,
			reach,
			special_attacks,
			special_qualities,
			saves,
			abilities,
			skills,
			bonus_feats,
			feats,
			epic_feats,
			environment,
			organization,
			challenge_rating,
			treasure,
			alignment,
			advancement,
			level_adjustment,
			special_abilities,
			stat_block,
			full_text,
			reference
		FROM monster
		WHERE name LIKE ? OR altname LIKE ?
	`, "%"+name+"%", "%"+name+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to query monsters: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var monster Monster
		if err := rows.Scan(
			&monster.ID,
			&monster.Family,
			&monster.Name,
			&monster.Altname,
			&monster.Size,
			&monster.Type,
			&monster.Descriptor,
			&monster.HitDice,
			&monster.Initiative,
			&monster.Speed,
			&monster.ArmorClass,
			&monster.BaseAttack,
			&monster.Grapple,
			&monster.Attack,
			&monster.FullAttack,
			&monster.Space,
			&monster.Reach,
			&monster.SpecialAttacks,
			&monster.SpecialQualities,
			&monster.Saves,
			&monster.Abilities,
			&monster.Skills,
			&monster.BonusFeats,
			&monster.Feats,
			&monster.EpicFeats,
			&monster.Environment,
			&monster.Organization,
			&monster.ChallengeRating,
			&monster.Treasure,
			&monster.Alignment,
			&monster.Advancement,
			&monster.LevelAdjustment,
			&monster.SpecialAbilities,
			&monster.StatBlock,
			&monster.FullText,
			&monster.Reference,
		); err != nil {
			return nil, fmt.Errorf("failed to scan monster: %v", err)
		}
		monsters = append(monsters, &monster)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over rows: %v", err)
	}

	if len(monsters) == 0 {
		return nil, nil
	}
	return monsters, nil
}
