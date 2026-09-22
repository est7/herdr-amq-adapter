package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CountMessages counts messages, not .dest sidecars. Only absence is empty.
func CountMessages(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read queue %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n, nil
}

func PendingSpools(root string) (map[string]int, error) {
	base := filepath.Join(root, "bridge", "outbox")
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read spools: %w", err)
	}
	out := map[string]int{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := CountMessages(filepath.Join(base, e.Name(), "new"))
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out[e.Name()] = n
		}
	}
	return out, nil
}
