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

var _ wheel.FixedStore = (*Journal)(nil)

// ArchiveFixedBatch archives only the final batch in this locked account's
// durable journal. Existing archives are verified, never replaced.
func (j *Journal) ArchiveFixedBatch(batch wheel.FixedBatch) (string, error) {
	f, err := PrivateRead(j.path)
	if err != nil {
		return "", err
	}
	var state wheel.State
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	err = d.Decode(&state)
	var extra any
	if err == nil && d.Decode(&extra) != io.EOF {
		err = errors.New("journal has trailing data")
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return "", err
	}
	if err = state.Validate(); err != nil {
		return "", err
	}
	if state.Fixed == nil || state.Fixed.Batch == nil || batch.Phase != "sealed" {
		return "", errors.New("fixed archive lacks its bound durable batch")
	}
	for _, signal := range batch.Signals {
		if signal.Phase == "ready" || signal.Phase == "unobserved" {
			return "", errors.New("fixed archive still has a possible future attempt")
		}
	}
	raw, err := json.MarshalIndent(batch, "", "  ")
	if err != nil || len(raw)+1 > 128<<10 {
		return "", errors.New("fixed batch exceeds archive bounds")
	}
	stored, err := json.MarshalIndent(state.Fixed.Batch, "", "  ")
	if err != nil || !bytes.Equal(raw, stored) {
		return "", errors.New("fixed archive differs from durable journal")
	}
	raw = append(raw, '\n')
	digest := sha256.Sum256(raw)
	pin := hex.EncodeToString(digest[:])
	dir := filepath.Join(filepath.Dir(j.path), "fixed-signal-archive")
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("fixed archive directory must be private and not a symlink")
	}
	path := filepath.Join(dir, batch.Day+"-"+pin+".json")
	file, err := os.CreateTemp(dir, ".fixed-batch-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return "", err
	}
	// Linking publishes complete, synced bytes atomically without overwriting
	// an existing immutable file. A failed write leaves no canonical archive.
	err = os.Link(file.Name(), path)
	if errors.Is(err, os.ErrExist) {
		old, readErr := PrivateRead(path)
		if readErr != nil {
			return "", readErr
		}
		data, readErr := io.ReadAll(io.LimitReader(old, (128<<10)+1))
		if readErr == nil && bytes.Equal(data, raw) {
			readErr = old.Sync()
		}
		readErr = errors.Join(readErr, old.Close())
		if readErr != nil || !bytes.Equal(data, raw) {
			return "", errors.New("immutable fixed archive content differs")
		}
	} else if err != nil {
		return "", err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	err = errors.Join(directory.Sync(), directory.Close())
	if err != nil {
		return "", err
	}
	parent, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return "", err
	}
	if err = errors.Join(parent.Sync(), parent.Close()); err != nil {
		return "", err
	}
	return pin, nil
}
