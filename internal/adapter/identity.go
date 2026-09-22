package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var herdrNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ChooseHandle picks the AMQ handle for an agent. A Herdr live name wins
// verbatim. Otherwise preferred (the handle this pane used before its agent
// restarted; Herdr drops the live name on release) is reused when no other
// live agent holds it, so mail addressed to the old handle still reaches the
// same pane. Otherwise the agent kind ("claude", "codex", …) is the base and
// a numeric suffix keeps it unique among the given taken names: claude,
// claude-2, claude-3 … The result always satisfies Herdr's name grammar so it
// can be written back with `herdr agent rename`.
func ChooseHandle(a AgentInfo, taken map[string]bool, preferred string) string {
	if name, ok := Handle(a); ok {
		return name
	}
	if preferred != "" && !taken[preferred] {
		return preferred
	}
	base := "agent"
	if a.Agent != nil && *a.Agent != "" {
		base = sanitizeName(*a.Agent)
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !taken[cand] {
			return cand
		}
	}
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		out = "a" + out
	}
	if len(out) > 32 {
		out = out[:32]
	}
	if !herdrNameRe.MatchString(out) {
		return "agent"
	}
	return out
}

// TakenNames collects live agent names so ChooseHandle stays unique.
func TakenNames(live []AgentInfo) map[string]bool {
	taken := map[string]bool{}
	for _, a := range live {
		if n, ok := Handle(a); ok {
			taken[n] = true
		}
	}
	return taken
}

// Identity is what an agent needs to use AMQ from inside its pane. It is
// written to a fixed, pane-addressable path so the agent (which cannot see
// the plugin's environment) can `source` it using its own HERDR_PANE_ID.
type Identity struct {
	PaneID string
	Handle string
	Root   string
}

func IdentityPath(configDir, paneID string) string {
	return filepath.Join(configDir, "panes", strings.ReplaceAll(paneID, ":", "_")+".env")
}

// Render is the sourceable shell form; values are single-quoted with any
// embedded quote escaped so a hostile cwd cannot break out.
func (id Identity) Render() string {
	return fmt.Sprintf("export AM_ROOT=%s\nexport AM_ME=%s\nexport HERDR_AMQ_PANE=%s\n",
		shellQuote(id.Root), shellQuote(id.Handle), shellQuote(id.PaneID))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func WriteIdentity(configDir string, id Identity) error {
	p := IdentityPath(configDir, id.PaneID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(id.Render()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func RemoveIdentity(configDir, paneID string) error {
	err := os.Remove(IdentityPath(configDir, paneID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// AgentsWith returns the agents listed in an amq config.json plus handle,
// sorted, and whether handle was missing. The caller re-runs
// `amq init --force --agents <list>` so amq itself creates the mailbox
// directories (inbox/new,cur,tmp, dlq, outbox/sent, receipts) with its own
// validation and fsync discipline; a waker started against a handle without
// those directories never acquires its inbox watcher.
func AgentsWith(configJSON []byte, handle string) ([]string, bool, error) {
	var cfg struct {
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, false, fmt.Errorf("decode amq config: %w", err)
	}
	for _, a := range cfg.Agents {
		if a == handle {
			return cfg.Agents, false, nil
		}
	}
	agents := append(append([]string{}, cfg.Agents...), handle)
	sort.Strings(agents)
	return agents, true, nil
}

// Notice is the text actually submitted to the agent: amq's own doorbell
// (which already says what to run) plus the identity it needs first. Kept to
// one line so it survives any input mode.
func Notice(payload string, id Identity, identityPath string) string {
	return fmt.Sprintf("%s (you are %s in Herdr; first: source %s)",
		strings.TrimSpace(payload), id.Handle, shellQuote(identityPath))
}
