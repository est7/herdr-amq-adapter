// Package durable publishes plugin-owned files before acknowledging persistence.
package durable

import (
	"errors"
	"os"
	"path/filepath"
)

func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// MkdirAll also persists newly created directory entries in their parents.
func MkdirAll(path string) error {
	if st, err := os.Stat(path); err == nil {
		if !st.IsDir() {
			return &os.PathError{Op: "mkdir", Path: path, Err: os.ErrExist}
		}
		return SyncDir(filepath.Dir(path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if err := MkdirAll(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return SyncDir(parent)
}

func WriteFile(path string, data []byte, mode os.FileMode) error {
	return writeFile(path, data, mode, func(f *os.File) error { return f.Sync() })
}

func writeFile(path string, data []byte, mode os.FileMode, sync func(*os.File) error) error {
	dir := filepath.Dir(path)
	if err := MkdirAll(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".publish-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := sync(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(sync(d), d.Close())
}
