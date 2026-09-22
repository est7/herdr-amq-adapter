package durable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRequiresFileAndDirectorySync(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "record")
			calls := 0
			fault := errors.New("sync fault")
			err := writeFile(p, []byte("committed"), 0600, func(f *os.File) error {
				calls++
				st, err := f.Stat()
				if err != nil {
					return err
				}
				if st.IsDir() != (calls == 2) {
					t.Fatal("wrong persistence order")
				}
				if calls == failAt {
					return fault
				}
				return f.Sync()
			})
			if !errors.Is(err, fault) || calls != failAt {
				t.Fatalf("sync failure swallowed: %v, calls %d", err, calls)
			}
			if failAt == 1 {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("published before file sync")
				}
			}
			if err := WriteFile(p, []byte("committed"), 0600); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(p)
			if err != nil || string(b) != "committed" {
				t.Fatalf("retry: %q %v", b, err)
			}
		})
	}
}
