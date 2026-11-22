//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogErrorRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	logError(link, "message")
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("target = %q, error = %v", data, err)
	}

	path := filepath.Join(dir, "permabox.log")
	logError(path, "first message")
	logError(path, "second message")
	data, err = os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "first message") || !strings.Contains(string(data), "second message") {
		t.Fatalf("log = %q, error = %v", data, err)
	}
}
