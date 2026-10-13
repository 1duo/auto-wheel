package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"autowheel/internal/wheel"
)

type Journal struct {
	path string
	lock *os.File
}

func OpenJournal(dir string) (*Journal, wheel.State, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, wheel.State{}, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, wheel.State{}, errors.New("state directory must be private (0700), not a symlink")
	}
	fd, err := syscall.Open(filepath.Join(dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, wheel.State{}, err
	}
	lock := os.NewFile(uintptr(fd), "lock")
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, wheel.State{}, errors.New("journal lock must be a private regular file")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, wheel.State{}, errors.New("another AutoWheel process owns this state directory")
	}
	j := &Journal{path: filepath.Join(dir, "journal.json"), lock: lock}
	st := wheel.NewState()
	f, err := PrivateRead(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return j, st, nil
	}
	if err != nil {
		j.Close()
		return nil, st, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&st); err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("journal has trailing content")
		}
	}
	if err == nil {
		err = st.Validate()
	}
	if err != nil {
		j.Close()
		return nil, st, fmt.Errorf("invalid journal: %w", err)
	}
	return j, st, nil
}

func (j *Journal) Save(st wheel.State) error {
	if err := st.Validate(); err != nil {
		return err
	}
	return AtomicJSON(j.path, st)
}
func (j *Journal) Close() error { return j.lock.Close() }

func PrivateRead(path string) (*os.File, error) {
	// Nonblocking open lets the regular-file check reject FIFOs without waiting
	// for a writer. It does not change reads from accepted regular files.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("secret/state file must be regular, private (0600), not a symlink")
	}
	return f, nil
}

func AtomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".autowheel-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
