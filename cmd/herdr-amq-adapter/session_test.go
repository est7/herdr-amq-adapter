package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/herdr-amq-adapter/internal/adapter"
)

func TestForeignSessionNeverTouchesLifecycle(t *testing.T) {
	d := t.TempDir()
	state := filepath.Join(d, "state")
	config := filepath.Join(d, "config")
	store, err := adapter.NewStore(state)
	if err != nil {
		t.Fatal(err)
	}
	rec := adapter.WakerRecord{PaneID: "w1:p1", Handle: "codex", ServerSocket: "/server-a.sock", Generation: "owned"}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	if err := adapter.WriteIdentity(config, adapter.Identity{PaneID: rec.PaneID, Handle: rec.Handle, Root: "/root-a"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(adapter.IdentityPath(config, rec.PaneID))
	if err != nil {
		t.Fatal(err)
	}
	called := filepath.Join(d, "called")
	fake := filepath.Join(d, "herdr")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+called+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", config)
	t.Setenv("HERDR_SOCKET_PATH", "/server-b.sock")
	t.Setenv("HERDR_BIN_PATH", fake)
	t.Setenv("AMQ_BIN", fake)
	for _, fn := range []func() error{runReconcile, func() error {
		t.Setenv("HERDR_PLUGIN_EVENT_JSON", `{"event":"pane.closed","data":{"pane_id":"w1:p1"}}`)
		return runHook()
	}} {
		if err := fn(); err == nil || !strings.Contains(err.Error(), "single-session") {
			t.Fatalf("foreign pass: %v", err)
		}
	}
	got, ok, err := store.Get(rec.PaneID)
	if err != nil || !ok || got.Generation != rec.Generation || got.ServerSocket != rec.ServerSocket {
		t.Fatalf("foreign record mutated: %+v %v", got, err)
	}
	after, err := os.ReadFile(adapter.IdentityPath(config, rec.PaneID))
	if err != nil || string(after) != string(before) {
		t.Fatal("foreign identity changed")
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatal("foreign pass reached Herdr")
	}
}
