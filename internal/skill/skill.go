// Package skill embeds the agent-facing SKILL.md so that the binary is the
// single source of truth for usage documentation.
package skill

import _ "embed"

// Name is the skill directory name.
const Name = "ds-tools"

// Markdown is the embedded SKILL.md.
//
//go:embed SKILL.md
var Markdown string
