package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// Stage in the destination directory, sync contents, publish atomically, then
// sync the directory entry. Unique staging names also isolate concurrent jobs.
func writeDurableFile(path string, data []byte) error {
	return writeDurableStream(path, func(w io.Writer) error { _, err := io.Copy(w, bytes.NewReader(data)); return err })
}

func writeDurableStream(path string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
