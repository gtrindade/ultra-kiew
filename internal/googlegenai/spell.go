package googlegenai

import (
	"fmt"
	"strings"

	"github.com/gtrindade/ultra-kiew/internal/mysql"
	"google.golang.org/genai"
)

const (
	// SpellLookupName is the name of the tool that looks up spell descriptions.
	SpellLookupToolName = "spell_lookup"
)

var (
	// SpellLookup is the name of the tool that provides spell descriptions.
	SpellLookupTool = &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        SpellLookupToolName,
				Description: "Provide descriptions for spells. If asked for a description of the spell, provide the full output of the function call.",
				Parameters: &genai.Schema{
					Type: "object",
					Properties: map[string]*genai.Schema{
						"spellName": {
							Type:        "string",
							Description: "The spell name",
							Example:     "What is the description for the spell Fireball?",
						},
					},
					Required: []string{"spellName"},
				},
			},
		},
	}
)

func (c *Client) SpellLookup(args map[string]any) (string, error) {
	spellName, ok := args["spellName"].(string)
	if !ok {
		return "", fmt.Errorf("invalid argument: spellName is required")
	}

	fmt.Printf("Looking up spell: %q\n", spellName)

	spells, err := c.dbClient.GetSpellByName(spellName)
	if err != nil {
		return "", fmt.Errorf("failed to get spell from database: %v", err)
	}

	if len(spells) == 0 {
		return fmt.Sprintf("No spell found with the name %q", spellName), nil
	}

	results := ""
	for _, spell := range spells {
		results += formatSpellDescription(spell)
	}

	if results == "" {
		return fmt.Sprintf("The spell %q was found but has no description", spellName), nil
	}

	return results, nil
}

func formatSpellDescription(spell *mysql.Spell) string {
	var desc strings.Builder

	// Header with name and school
	desc.WriteString(fmt.Sprintf("%s\n", spell.Name))
	if spell.SubSchool != "" {
		desc.WriteString(fmt.Sprintf("%s [%s]\n", spell.School, spell.SubSchool))
	} else {
		desc.WriteString(fmt.Sprintf("%s\n", spell.School))
	}
	desc.WriteString("\n")

	// Level information
	if spell.ClassLevels != "" {
		desc.WriteString(fmt.Sprintf("Level: %s\n", spell.ClassLevels))
	}

	// Components
	if spell.Components != "" {
		desc.WriteString(fmt.Sprintf("Components: %s\n", spell.Components))
	}

	// Spell characteristics
	if spell.CastingTime != nil && *spell.CastingTime != "" {
		desc.WriteString(fmt.Sprintf("Casting Time: %s\n", *spell.CastingTime))
	}
	if spell.Range != nil && *spell.Range != "" {
		desc.WriteString(fmt.Sprintf("Range: %s\n", *spell.Range))
	}
	if spell.Effect != nil && *spell.Effect != "" {
		desc.WriteString(fmt.Sprintf("Effect: %s\n", *spell.Effect))
	}
	if spell.Area != nil && *spell.Area != "" {
		desc.WriteString(fmt.Sprintf("Area: %s\n", *spell.Area))
	}
	if spell.Duration != nil && *spell.Duration != "" {
		desc.WriteString(fmt.Sprintf("Duration: %s\n", *spell.Duration))
	}
	if spell.SavingThrow != nil && *spell.SavingThrow != "" {
		desc.WriteString(fmt.Sprintf("Saving Throw: %s\n", *spell.SavingThrow))
	}
	if spell.SpellResistance != nil && *spell.SpellResistance != "" {
		desc.WriteString(fmt.Sprintf("Spell Resistance: %s\n", *spell.SpellResistance))
	}

	// Description
	desc.WriteString("\n")
	desc.WriteString(spell.Description)

	// Source
	if spell.Source != "" {
		desc.WriteString(fmt.Sprintf("\n\nSource: %s", spell.Source))
	}

	return desc.String()
}
