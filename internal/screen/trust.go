package screen

import (
	"strings"
	"unicode"
)

// trustPhrases are lines only a trust screen draws: a "trust this folder?"
// dialog, a restricted-folder chooser, or a hooks or settings review. The
// harness saves the answer for every later session in that folder, and
// Herdr does not always report these screens as blocked, so a prompt ending
// in Enter would pick the preselected "trust" option.
var trustPhrases = []string{
	// Claude Code
	"Quick safety check: Is this a project you created or one you trust",
	"Do you trust the files in this folder?",
	"Yes, I trust this folder",
	"Yes, I trust these settings",
	// Codex
	"Trust this folder?",
	"Do you trust the contents of this directory?",
	"Hooks need review",
	"Trust all and continue",
	"Your trust decision will be saved",
	// Gemini CLI and Qwen Code
	"Do you trust this folder?",
	// Cursor
	"Workspace Trust Required",
	// Copilot CLI
	"Confirm folder trust",
}

// TrustScreen reports the trust-dialog phrase on screen. A screen whose
// input box is visible and empty is the agent's prompt, and a phrase on it
// is only transcript text (an agent quoting a dialog).
func TrustScreen(kind, screen string) (string, bool) {
	if Check(kind, screen) == Empty {
		return "", false
	}
	plain := plainText(screen)
	for _, p := range trustPhrases {
		if strings.Contains(plain, p) {
			return p, true
		}
	}
	return "", false
}

// plainText drops escape sequences and joins runs of whitespace, so a
// phrase drawn with styled or padded words still matches.
func plainText(screen string) string {
	var b strings.Builder
	space := false
	rs := []rune(screen)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		switch {
		case ch == 0x1b:
			if i+1 < len(rs) && rs[i+1] == '[' {
				for i += 2; i < len(rs) && (rs[i] < 0x40 || rs[i] > 0x7e); i++ {
				}
			} else {
				i++
			}
		case ch == '\n':
			b.WriteByte('\n')
			space = false
		case unicode.IsSpace(ch) || unicode.IsControl(ch):
			if !space {
				b.WriteByte(' ')
				space = true
			}
		default:
			b.WriteRune(ch)
			space = false
		}
	}
	return b.String()
}
