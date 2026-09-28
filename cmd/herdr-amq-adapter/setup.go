package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const adapterRepo = "est7/herdr-amq-adapter"

// linkOutcome is what ensureLink did.
type linkOutcome string

const (
	linkCreated linkOutcome = "linked"
	linkUpdated linkOutcome = "relinked"
	linkKept    linkOutcome = "already linked"
	linkLeft    linkOutcome = "left alone"
)

// ensureLink points link at target. A missing link is created; a link of
// ours (replace reports whether an existing link target is one) is updated;
// anything else is the user's and is left alone.
func ensureLink(link, target string, replace func(current string) bool) (linkOutcome, string, error) {
	st, err := os.Lstat(link)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return "", "", err
		}
		return linkCreated, target, os.Symlink(target, link)
	case err != nil:
		return "", "", err
	case st.Mode()&os.ModeSymlink == 0:
		return linkLeft, "not a link", nil
	}
	current, err := os.Readlink(link)
	if err != nil {
		return "", "", err
	}
	if current == target {
		return linkKept, target, nil
	}
	if !replace(current) {
		return linkLeft, "links " + current, nil
	}
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return "", "", err
	}
	return linkUpdated, target, os.Rename(tmp, link)
}

// ourBinary reports whether a command link points at a copy of this plugin
// (a Herdr-managed install or a checkout's bin/), or at nothing any more.
func ourBinary(current string) bool {
	if _, err := os.Stat(current); err != nil {
		return true
	}
	return strings.HasSuffix(current, "/bin/herdr-amq-adapter")
}

// ourSkillLink reports whether an existing skill link may be repointed:
// it is broken, or it points into Herdr's plugin folder (an earlier install
// of this plugin). A link a skills manager made is someone else's.
func ourSkillLink(current string) bool {
	if _, err := os.Stat(current); err != nil {
		return true
	}
	return strings.Contains(current, "/herdr/plugins/")
}

// runConfigure links the command and the agent skill, then adopts the
// agents already open. It is safe to run again.
func runConfigure() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(self)) // <plugin root>/bin/herdr-amq-adapter
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := os.Getenv("XDG_BIN_HOME")
	if binDir == "" {
		binDir = filepath.Join(home, ".local", "bin")
	}
	cmdLink := filepath.Join(binDir, "herdr-amq-adapter")
	out, note, err := ensureLink(cmdLink, self, ourBinary)
	if err != nil {
		return fmt.Errorf("link %s: %w", cmdLink, err)
	}
	fmt.Printf("command: %s %s (%s)\n", cmdLink, out, note)
	if !strings.Contains(":"+os.Getenv("PATH")+":", ":"+binDir+":") {
		fmt.Printf("  %s is not on PATH; add it to use `herdr-amq-adapter` in a shell\n", binDir)
	}

	skill := filepath.Join(root, "skills", "herdr-amq-adapter")
	if _, err := os.Stat(filepath.Join(skill, "SKILL.md")); err != nil {
		return fmt.Errorf("skill not found at %s: %w", skill, err)
	}
	// Claude Code always; Codex when it is installed. A skill of the same
	// name that is not ours (another skills manager) is kept.
	dirs := []string{filepath.Join(home, ".claude", "skills")}
	if st, err := os.Stat(filepath.Join(home, ".codex")); err == nil && st.IsDir() {
		dirs = append(dirs, filepath.Join(home, ".codex", "skills"))
	}
	for _, d := range dirs {
		link := filepath.Join(d, "herdr-amq-adapter")
		out, note, err := ensureLink(link, skill, ourSkillLink)
		if err != nil {
			return fmt.Errorf("link %s: %w", link, err)
		}
		fmt.Printf("skill: %s %s (%s)\n", link, out, note)
	}

	if os.Getenv("HERDR_SOCKET_PATH") == "" {
		fmt.Println("not inside Herdr: run `herdr plugin action invoke est7.amq-adapter.reconcile` to adopt open agents")
		return nil
	}
	return runReconcile()
}

// runUpdate reinstalls the newest release through Herdr and adopts agents
// with the new binary. A linked checkout updates from its own git instead.
func runUpdate(args []string) error {
	herdr := adapterHerdrBin()
	out, err := exec.Command(herdr, "plugin", "list", "--plugin", pluginID, "--json").Output()
	if err != nil {
		return fmt.Errorf("herdr plugin list: %w", err)
	}
	kind, err := pluginSourceKind(out)
	if err != nil {
		return err
	}
	if kind != "github" {
		return fmt.Errorf("this plugin is linked from a local folder (%s); update it there: git pull && sh scripts/install.sh, then run the reconcile action", kind)
	}
	tags, err := exec.Command("git", "ls-remote", "--tags", "--refs", "https://github.com/"+adapterRepo).Output()
	if err != nil {
		return fmt.Errorf("list releases: %w", err)
	}
	latest := newestTag(string(tags))
	if latest == "" {
		return errors.New("no vX.Y.Z release tag found on GitHub")
	}
	if len(args) > 0 && args[0] == "--check" {
		fmt.Printf("installed: %s\nnewest:    %s\n", version, strings.TrimPrefix(latest, "v"))
		return nil
	}
	if strings.TrimPrefix(latest, "v") == version {
		fmt.Printf("already on the newest release %s\n", latest)
		return nil
	}
	cmd := exec.Command(herdr, "plugin", "install", adapterRepo, "--ref", latest, "--yes")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("herdr plugin install %s --ref %s: %w (the installed version is unchanged)", adapterRepo, latest, err)
	}
	// The new binary adopts agents and replaces wakers that still point at
	// the old one.
	cmd = exec.Command(herdr, "plugin", "action", "invoke", pluginID+".reconcile")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func adapterHerdrBin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}

func pluginSourceKind(listJSON []byte) (string, error) {
	var doc any
	if err := json.Unmarshal(listJSON, &doc); err != nil {
		return "", fmt.Errorf("decode herdr plugin list: %w", err)
	}
	var find func(any) string
	find = func(v any) string {
		switch t := v.(type) {
		case map[string]any:
			if t["plugin_id"] == pluginID {
				if src, ok := t["source"].(map[string]any); ok {
					if k, ok := src["kind"].(string); ok {
						return k
					}
				}
			}
			for _, c := range t {
				if k := find(c); k != "" {
					return k
				}
			}
		case []any:
			for _, c := range t {
				if k := find(c); k != "" {
					return k
				}
			}
		}
		return ""
	}
	if k := find(doc); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("%s is not installed in this Herdr", pluginID)
}

// newestTag picks the highest vX.Y.Z from `git ls-remote --tags --refs`.
func newestTag(lsRemote string) string {
	type semver struct {
		tag string
		n   [3]int
	}
	var all []semver
	for _, line := range strings.Split(lsRemote, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		tag := strings.TrimPrefix(f[1], "refs/tags/")
		parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
		if !strings.HasPrefix(tag, "v") || len(parts) != 3 {
			continue
		}
		var s semver
		ok := true
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				ok = false
				break
			}
			s.n[i] = n
		}
		if ok {
			s.tag = tag
			all = append(all, s)
		}
	}
	if len(all) == 0 {
		return ""
	}
	sort.Slice(all, func(i, j int) bool {
		for k := 0; k < 3; k++ {
			if all[i].n[k] != all[j].n[k] {
				return all[i].n[k] < all[j].n[k]
			}
		}
		return false
	})
	return all[len(all)-1].tag
}

// fileID tells a binary replaced at the same path (Herdr's install swap, a
// rebuild) from the one a long-lived process started from.
type fileID struct {
	ino     uint64
	size    int64
	modNano int64
}

func binaryID(path string) (fileID, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fileID{}, err
	}
	id := fileID{size: st.Size(), modNano: st.ModTime().UnixNano()}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		id.ino = uint64(sys.Ino)
	}
	return id, nil
}

// applyPlan retires and adopts panes one by one. A failure in one pane is
// reported and the rest still run: one bad record must not leave every
// other agent unadopted.
func applyPlan(stops, starts []string, stop, ensure func(string) error) error {
	var errs []error
	for _, p := range stops {
		if err := stop(p); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", p, err))
		}
	}
	for _, p := range starts {
		if err := ensure(p); err != nil {
			errs = append(errs, fmt.Errorf("ensure %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}
