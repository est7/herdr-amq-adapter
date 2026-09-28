package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewestTag(t *testing.T) {
	out := "a\trefs/tags/v0.2.9\nb\trefs/tags/v0.10.0\nc\trefs/tags/v0.3.1\nd\trefs/tags/nightly\ne\trefs/tags/v1.0\n"
	if got := newestTag(out); got != "v0.10.0" {
		t.Fatalf("got %q", got)
	}
	if got := newestTag("x\trefs/tags/latest\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestPluginSourceKind(t *testing.T) {
	js := []byte(`[{"plugin_id":"other","source":{"kind":"local"}},{"plugin_id":"est7.amq-adapter","source":{"kind":"github","owner":"est7"}}]`)
	if k, err := pluginSourceKind(js); err != nil || k != "github" {
		t.Fatalf("got %q %v", k, err)
	}
	if _, err := pluginSourceKind([]byte(`[]`)); err == nil {
		t.Fatal("a missing plugin must be an error")
	}
}

// ensureLink creates or updates links it owns and never touches a file,
// or a link that belongs to someone else.
func TestEnsureLink(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "new", "bin", "herdr-amq-adapter")
	never := func(string) bool { return false }
	always := func(string) bool { return true }

	link := filepath.Join(d, "bin", "a")
	if out, _, err := ensureLink(link, target, never); err != nil || out != linkCreated {
		t.Fatalf("create: %v %v", out, err)
	}
	if out, _, _ := ensureLink(link, target, never); out != linkKept {
		t.Fatalf("again: %v", out)
	}
	other := filepath.Join(d, "other")
	if out, _, _ := ensureLink(link, other, never); out != linkLeft {
		t.Fatalf("foreign link replaced: %v", out)
	}
	if out, _, err := ensureLink(link, other, always); err != nil || out != linkUpdated {
		t.Fatalf("own link: %v %v", out, err)
	}
	if got, _ := os.Readlink(link); got != other {
		t.Fatalf("points at %q", got)
	}
	file := filepath.Join(d, "bin", "file")
	if err := os.WriteFile(file, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := ensureLink(file, target, always); out != linkLeft {
		t.Fatalf("regular file replaced: %v", out)
	}
	if b, _ := os.ReadFile(file); string(b) != "mine" {
		t.Fatal("file changed")
	}
}

// A skill link a skills manager made is never repointed; a broken one or
// one into Herdr's plugin folder (an earlier install) is.
func TestOurSkillLink(t *testing.T) {
	d := t.TempDir()
	vendor := filepath.Join(d, ".agents/resources/skills/vendor/shared/est7/herdr-amq-adapter/skills/herdr-amq-adapter")
	plugin := filepath.Join(d, ".config/herdr/plugins/github/est7.amq-adapter-abc/skills/herdr-amq-adapter")
	for _, p := range []string{vendor, plugin} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ourSkillLink(vendor) {
		t.Error("a skills manager's link would be replaced")
	}
	if !ourSkillLink(plugin) {
		t.Error("an earlier plugin install's link would be kept")
	}
	if !ourSkillLink(filepath.Join(d, "gone")) {
		t.Error("a broken link would be kept")
	}
}
