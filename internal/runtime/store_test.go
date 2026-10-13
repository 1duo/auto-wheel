package runtime

import (
	"autowheel/internal/wheel"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestJournalLockPrivateFilesAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	j, st, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, _, err := OpenJournal(dir); err == nil {
		other.Close()
		t.Fatal("second writer acquired lock")
	}
	st.Account = "12345678"
	st.Positions["NVDA"] = wheel.Position{OwnedShares: 100, Cost: 20000, Provenance: "receipt-123"}
	if err = j.Save(st); err != nil {
		t.Fatal(err)
	}
	j.Close()
	j, got, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	if got.Positions["NVDA"].Cost != 20000 {
		t.Fatal("restart lost ownership")
	}
	p := filepath.Join(dir, "journal.json")
	if err = os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if f, err := PrivateRead(p); err == nil {
		f.Close()
		t.Fatal("public state accepted")
	}
	if err = os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err = os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if f, err := PrivateRead(link); err == nil {
		f.Close()
		t.Fatal("secret symlink accepted")
	}
	if err = os.WriteFile(p, []byte(`{"version":1`), 0600); err != nil {
		t.Fatal(err)
	}
	if j, _, err := OpenJournal(dir); err == nil {
		j.Close()
		t.Fatal("torn journal accepted")
	}
}

func TestPrivateFilesRejectFIFOAndInvalidLock(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(pipe, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := PrivateRead(pipe); err == nil {
		f.Close()
		t.Fatal("FIFO accepted as private state")
	}
	for _, kind := range []string{"public", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			stateDir := t.TempDir()
			lock := filepath.Join(stateDir, "lock")
			var err error
			if kind == "fifo" {
				err = syscall.Mkfifo(lock, 0600)
			} else {
				err = os.WriteFile(lock, nil, 0644)
				if err == nil {
					err = os.Chmod(lock, 0644)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if j, _, err := OpenJournal(stateDir); err == nil {
				j.Close()
				t.Fatal("invalid operational lock accepted")
			}
		})
	}
}
