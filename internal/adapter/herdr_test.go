package adapter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPromptUsesServerBlockedDecision(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         Progress
	}{
		{"accepted", "exit 0", ProgressAccepted},
		{"blocked", `echo '{"error":{"code":"agent_blocked","message":"approval"}}' >&2; exit 1`, ProgressDeferred},
		{"missing", `echo '{"error":{"code":"agent_not_found"}}' >&2; exit 1`, ProgressFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "herdr")
			script := "#!/bin/sh\n[ \"$1\" = agent ] && [ \"$2\" = prompt ] || exit 9\n" + tc.script + "\n"
			if err := os.WriteFile(p, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			out, err := (Herdr{Bin: p}).Prompt("claude", "doorbell", time.Second)
			if err != nil || out.Progress != tc.want {
				t.Fatalf("got %+v %v", out, err)
			}
		})
	}
}
