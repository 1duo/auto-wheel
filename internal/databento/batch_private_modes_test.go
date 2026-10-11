package databento

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBatchEvidenceRejectsSpecialModes(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, special := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		if err := os.Chmod(directory, 0700|special); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(directory)
		if err != nil || info.Mode()&special == 0 {
			t.Fatalf("special directory mode not established: %v", err)
		}
		root, err := privateBatchRoot(directory)
		if root != nil {
			root.Close()
		}
		if !errors.Is(err, errBatchProof) {
			t.Fatal("special directory mode accepted")
		}
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := privateBatchRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path := filepath.Join(directory, "evidence")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, special := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		if err := os.Chmod(path, 0600|special); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode()&special == 0 {
			t.Fatalf("special file mode not established: %v", err)
		}
		file, err := privateBatchFile(root, "evidence")
		if file != nil {
			file.Close()
		}
		if !errors.Is(err, errBatchProof) {
			t.Fatal("special file mode accepted")
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := privateBatchFile(root, "evidence")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
}

func TestNativeBatchRejectsSpecialInterpreterBeforeExec(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "executed")
	python := filepath.Join(directory, "interpreter")
	quotedMarker := "'" + strings.ReplaceAll(marker, "'", "'\\''") + "'"
	if err := os.WriteFile(python, []byte("#!/bin/sh\nprintf x > "+quotedMarker+"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	query := BatchQuery{Dataset: "OPRA.PILLAR", Schema: "cmbp-1"}
	_, _ = runNativeBatch(t.Context(), python, query, directory, nil, 1)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("ordinary interpreter control did not execute", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	for _, special := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		if err := os.Chmod(python, 0700|special); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(python)
		if err != nil || info.Mode()&special == 0 {
			t.Fatalf("special interpreter mode not established: %v", err)
		}
		_, err = runNativeBatch(t.Context(), python, query, directory, nil, 1)
		if !errors.Is(err, errBatchProof) {
			t.Fatal("special interpreter accepted", err)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("special interpreter was executed", err)
		}
	}
}
