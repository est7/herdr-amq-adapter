package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A binary replaced at the same path (Herdr's install swap, a rebuild) is
// seen as a different file; an untouched one is not.
func TestBinaryIDNoticesAReplacedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := binaryID(p)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := binaryID(p); b != a {
		t.Fatal("the same file reads as changed")
	}
	next := p + ".new"
	if err := os.WriteFile(next, []byte("v2-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.Rename(next, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := binaryID(p); b == a {
		t.Fatal("a replaced binary reads as unchanged")
	}
}

// One failing pane never stops the others; every error is reported.
func TestApplyPlanContinuesPastErrors(t *testing.T) {
	var done []string
	stop := func(p string) error {
		done = append(done, "stop "+p)
		if p == "w1:bad" {
			return errors.New("retire refused")
		}
		return nil
	}
	ensure := func(p string) error {
		done = append(done, "ensure "+p)
		if p == "w2:bad" {
			return errors.New("herdr timeout")
		}
		return nil
	}
	err := applyPlan([]string{"w1:bad", "w1:ok"}, []string{"w2:bad", "w2:ok"}, stop, ensure)
	want := []string{"stop w1:bad", "stop w1:ok", "ensure w2:bad", "ensure w2:ok"}
	if !reflect.DeepEqual(done, want) {
		t.Fatalf("ran %v", done)
	}
	if err == nil || !strings.Contains(err.Error(), "retire refused") || !strings.Contains(err.Error(), "herdr timeout") {
		t.Fatalf("err %v", err)
	}
}
