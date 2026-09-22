package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/herdr-amq-adapter/internal/bridge"
)

func TestPopupReportsStaleAndUnavailableEvidence(t *testing.T) {
	v := popupView{At: time.Now(), Agents: []agentView{{Handle: "codex", Pane: "w1:p1", Wake: "valid", Pending: 2}}, Bridge: bridgeView{Configured: true, Running: true, Runner: &runnerStatus{LastTick: time.Now().Add(-time.Minute)}, Errors: []string{"permission denied\x1b[2J"}}}
	text := strings.Join(popupLines(v), "\n")
	for _, want := range []string{"codex", "valid / 2", "进度陈旧", "spool 待推送: 读取失败", "permission denied"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in %s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatal("terminal escape escaped from error data")
	}
}

func TestBridgeInspectionExposesCorruptStatus(t *testing.T) {
	d := t.TempDir()
	e := env{stateDir: d, root: filepath.Join(d, "root")}
	if err := os.MkdirAll(filepath.Dir(statusPath(e)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath(e), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	v := inspectBridge(context.Background(), e, bridge.Snapshot{Configured: true, Local: bridge.Local{Host: "mac"}})
	if v.Runner != nil || len(v.Errors) != 1 || !strings.Contains(v.Errors[0], "decode runner status") {
		t.Fatalf("corrupt status hidden: %+v", v)
	}
}

func TestPopupWrapKeepsFullError(t *testing.T) {
	input := "  错误: connection reset by peer"
	wrapped := wrapLines([]string{input}, 12)
	if strings.Join(wrapped, "") != input || len(wrapped) < 2 {
		t.Fatalf("error text lost: %v", wrapped)
	}
	if got := cropLine("状态abcd", 6); got != "状态ab" {
		t.Fatalf("column width: %s", got)
	}
}
