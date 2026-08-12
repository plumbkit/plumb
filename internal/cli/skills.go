package cli

import (
	"embed"
)

// The shipped library is SEVEN skills, and the number is a deliberate ceiling
// rather than an accident. The original plan called it a hard stop of 4-6, but
// that range was arithmetic (three shipped plus three planned) written before
// plumb-minimal-change landed, not a measured limit. What the ceiling is
// actually protecting is trigger quality: only a skill's name and description
// are always in context, so the cost of one more is a line, but the cost of a
// blurry one is the model firing the wrong skill — or none.
//
// The three added last are trigger-DISJOINT from the four before them: memory
// (recall or record something durable), diagnose (a call was refused or a result
// looks wrong), git (version control). Folding any of them into an existing
// skill would file its description under a trigger phrase that does not match —
// "refactor" does not fire on "commit this" — which is the failure the ceiling
// exists to prevent, not an instance of respecting it. Anthropic's own skills
// guidance argues the same way: split mutually exclusive content, do not grow
// one mega-skill.
//
// So: seven, and the eighth needs its own justification against this paragraph.
//
// plumb-chat is that eighth, and here is its argument. The test is trigger
// disjointness, and the mailbox has no trigger overlap with anything above: it
// fires on "another agent is working here and I need to reach it", which is not
// a refusal (diagnose), not recall (memory), not version control (git), and not
// a code operation at all. It is also the one lane where the cost of NOT having
// a skill is measurable rather than aesthetic — the mailbox's three hardest
// facts (delivery is poll-only, so silence is not refusal; the exchange cap
// bounds one thread, not one conversation; cross-project delivery is the
// recipient's gate and fails silently) are each a wrong default assumption
// away from an agent escalating at a peer that never saw its message, or
// talking to it forever. Folding those into plumb-explore or plumb-memory
// would file them under a trigger phrase that does not match, which is the
// failure the ceiling exists to prevent.
//
// So: eight, and the ninth needs its own justification against both paragraphs.
//
//go:embed skills
var skillsFS embed.FS

// embeddedSkill is a named skill file to be installed into a client's skills
// directory.
type embeddedSkill struct {
	Name    string
	Content string
}

// embeddedSkills returns the shipped skills, one per subdirectory of the
// embedded skills/ tree. The set is client-independent: WHICH clients receive it
// is setup_skills.go's question (setupTarget.skillsDirFn), not this file's.
func embeddedSkills() []embeddedSkill {
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
