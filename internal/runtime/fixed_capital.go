package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"autowheel/internal/wheel"
)

// ArchiveFixedCapitalState preserves the exact durable source journal before a
// paused capital rebind. It cannot replace an existing archive or save a state.
func (j *Journal) ArchiveFixedCapitalState(s wheel.State) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	f, err := PrivateRead(j.path)
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	err = errors.Join(err, f.Close())
	if err != nil || len(raw) > 1<<20 {
		return "", errors.New("capital archive cannot read bounded durable source")
	}
	var stored wheel.State
	if err = json.Unmarshal(raw, &stored); err != nil {
		return "", err
	}
	want, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	actual, err := json.Marshal(stored)
	if err != nil || !bytes.Equal(want, actual) {
		return "", errors.New("capital archive differs from locked durable journal")
	}
	digest := sha256.Sum256(raw)
	pin := hex.EncodeToString(digest[:])
	dir := filepath.Join(filepath.Dir(j.path), "fixed-capital-archive")
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("capital archive directory must be private and not a symlink")
	}
	path := filepath.Join(dir, pin+".json")
	temporary, err := os.CreateTemp(dir, ".capital-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	_, err = temporary.Write(raw)
	if err == nil {
		err = temporary.Sync()
	}
	if err = errors.Join(err, temporary.Close()); err != nil {
		return "", err
	}
	if err = os.Link(temporary.Name(), path); errors.Is(err, os.ErrExist) {
		old, readErr := PrivateRead(path)
		if readErr != nil {
			return "", readErr
		}
		content, readErr := io.ReadAll(io.LimitReader(old, (1<<20)+1))
		readErr = errors.Join(readErr, old.Close())
		if readErr != nil || !bytes.Equal(content, raw) {
			return "", errors.New("immutable capital source archive changed")
		}
	} else if err != nil {
		return "", err
	}
	for _, folder := range []string{dir, filepath.Dir(dir)} {
		directory, err := os.Open(folder)
		if err != nil {
			return "", err
		}
		if err = errors.Join(directory.Sync(), directory.Close()); err != nil {
			return "", err
		}
	}
	return pin, nil
}
