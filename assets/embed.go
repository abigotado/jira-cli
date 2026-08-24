// Package assets contains the provider-neutral Agent Skill shipped by jira-cli.
package assets

import "embed"

// FS contains every file under skills, including dotfiles if the package gains
// any later. Installer tests compare the embedded payload with the source tree
// so a packaging omission cannot silently ship an incomplete skill.
//
//go:embed all:skills
var FS embed.FS
