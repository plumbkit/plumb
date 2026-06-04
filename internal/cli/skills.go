package cli

import (
	"embed"
)

//go:embed skills
var skillsFS embed.FS

// embeddedSkill is a named skill file to be installed into a client's skills
// directory.
type embeddedSkill struct {
	Name    string
	Content string
}

// claudeCodeSkills returns the list of skills to install for Claude Code.
// Each skill maps to a subdirectory of the embedded skills/ tree.
func claudeCodeSkills() []embeddedSkill {
	entries, err := skillsFS.ReadDir("skills")
	if err != nil {
		return nil
	}
	var skills []embeddedSkill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		data, err := skillsFS.ReadFile("skills/" + name + "/SKILL.md")
		if err != nil {
			continue
		}
		skills = append(skills, embeddedSkill{Name: name, Content: string(data)})
	}
	return skills
}
