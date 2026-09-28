package screen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEverySupportedKindTellsAnEmptyBoxFromADraft(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "cursor", "gemini", "opencode", "pi"} {
		if got := Check(kind, fixture(t, kind+"-empty")); got != Empty {
			t.Errorf("%s empty: got %v", kind, got)
		}
		if got := Check(kind, fixture(t, kind+"-draft")); got != Typed {
			t.Errorf("%s draft: got %v", kind, got)
		}
	}
	for _, tc := range []struct{ kind, name string }{
		{"claude", "claude-empty-after-turn"},
		{"opencode", "opencode-empty-session"},
	} {
		if got := Check(tc.kind, fixture(t, tc.name)); got != Empty {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

// Claude keeps its input box while working: the dim "Press up to edit queued
// messages" hint is not a draft, text the user types meanwhile is.
func TestClaudeWhileWorking(t *testing.T) {
	for name, want := range map[string]Draft{
		"claude-working":        Empty,
		"claude-working-queued": Empty,
		"claude-working-draft":  Typed,
	} {
		if got := Check("claude", fixture(t, name)); got != want {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
}

func TestScreenWithoutTheBoxOrUnknownKindIsUnknown(t *testing.T) {
	if got := Check("copilot", fixture(t, "claude-empty")); got != Unknown {
		t.Errorf("unknown kind: got %v", got)
	}
	if got := Check("claude", "some output\nno box here\n"); got != Unknown {
		t.Errorf("no box: got %v", got)
	}
	if got := Check("codex", fixture(t, "claude-empty")); got != Unknown {
		t.Errorf("wrong kind layout: got %v", got)
	}
}

func TestMenusAndMultiLineDraftsCountAsTyped(t *testing.T) {
	rule := strings.Repeat("─", 40)
	for name, tc := range map[string]struct {
		kind, screen string
		want         Draft
	}{
		// Codex's trust menu uses the same `›` for its selection.
		"codex menu":  {"codex", "  Do you trust the contents of this directory?\n\x1b[1m›\x1b[0m 1. Yes, continue\n  2. No, quit\n", Typed},
		"multi-line":  {"claude", rule + "\n❯ \n  second line typed\n" + rule + "\n", Typed},
		"placeholder": {"claude", rule + "\n❯ \x1b[2mTry \"fix lint\"\x1b[0m\n" + rule + "\n", Empty},
		// A transcript line with `❯` that is not under a rule is not the box.
		"transcript": {"claude", "❯ an earlier prompt\n\n" + rule + "\n❯ \n" + rule + "\n", Empty},
		// A typed character under the harness cursor is text; a cursor on a
		// dim placeholder is not.
		"cursor on text":        {"pi", rule + "\n\x1b[7mx\x1b[0m   \n" + rule + "\n", Typed},
		"cursor on placeholder": {"cursor", "  \x1b[2m→ \x1b[0m\x1b[7mP\x1b[0m\x1b[2mlan, search\x1b[0m\n", Empty},
	} {
		if got := Check(tc.kind, tc.screen); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

func TestEachHarnessTrustScreenIsRecognized(t *testing.T) {
	for name, tc := range map[string]struct{ kind, screen, want string }{
		"claude":       {"claude", "╭────╮\n│ Accessing workspace:\n│ Quick safety check: Is this a project you created or one you trust? (Like your own code)\n│ \x1b[1m❯\x1b[0m 1. Yes, I trust this folder\n│   2. No, exit\n╰────╯\n", "Quick safety check: Is this a project you created or one you trust"},
		"codex folder": {"codex", "  Trust this folder? Codex can read, edit, and run files here.\n\x1b[1m›\x1b[0m 1. Trust and continue\n  2. Open restricted\n", "Trust this folder?"},
		"codex hooks":  {"codex", "  Hooks need review\n  1 hook is new or changed.\n› 1. Review hooks\n  2. Trust all and continue\n", "Hooks need review"},
		"old codex":    {"codex", "  Do you trust the contents of this directory?\n\x1b[1m›\x1b[0m 1. Yes, continue\n  2. No, quit\n", "Do you trust the contents of this directory?"},
		"gemini":       {"gemini", "│ Do you trust this folder?\n│ ● 1. Trust folder (demo)\n", "Do you trust this folder?"},
		"copilot":      {"copilot", "Confirm folder trust\n❯ 1. Yes\n", "Confirm folder trust"},
		// Styling and padding between words do not hide it.
		"styled": {"kiro", "Hooks \x1b[1mneed\x1b[0m   review\n", "Hooks need review"},
	} {
		if got, ok := TrustScreen(tc.kind, tc.screen); !ok || got != tc.want {
			t.Errorf("%s: got %q %v want %q", name, got, ok, tc.want)
		}
	}
}

func TestOrdinaryScreensAndQuotedPhrasesAreNotTrustScreens(t *testing.T) {
	rule := strings.Repeat("─", 40)
	for name, tc := range map[string]struct{ kind, screen string }{
		// A transcript quoting the dialog above an empty input box.
		"quoted":     {"claude", "the dialog said \"Do you trust the files in this folder?\"\n" + rule + "\n❯ \n" + rule + "\n"},
		"sign in":    {"codex", "  Welcome to Codex\n› 1. Sign in with ChatGPT\n"},
		"permission": {"claude", "Allow this edit?\n❯ 1. Yes\n  2. No\n"},
		"empty":      {"claude", ""},
		"idle":       {"claude", fixture(t, "claude-empty")},
	} {
		if got, ok := TrustScreen(tc.kind, tc.screen); ok {
			t.Errorf("%s: detected %q", name, got)
		}
	}
}

// A truncated OpenCode box (two `┃` rows, the first empty, then `╹`) must
// read as a layout, not crash the injector.
func TestTruncatedOpencodeBoxDoesNotPanic(t *testing.T) {
	if got := Check("opencode", "┃\n┃ \n╹▀▀▀\n"); got != Empty {
		t.Errorf("got %v", got)
	}
}
