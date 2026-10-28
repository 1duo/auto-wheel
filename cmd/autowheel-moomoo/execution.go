package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"autowheel/internal/moomoo"
)

var executableDigest struct {
	sync.Mutex
	info   os.FileInfo
	digest string
}

// executableHash re-hashes only when the executable's identity, size or
// modification time changes; every order checks it under the dispatch lock.
func executableHash() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	executableDigest.Lock()
	defer executableDigest.Unlock()
	if prior := executableDigest.info; prior != nil && os.SameFile(prior, info) && prior.Size() == info.Size() && prior.ModTime().Equal(info.ModTime()) {
		return executableDigest.digest, nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	executableDigest.info, executableDigest.digest = info, hex.EncodeToString(hash.Sum(nil))
	return executableDigest.digest, nil
}

func nativeExecution(config moomoo.Config, store *moomoo.State, dir, configPath string, source moomoo.ExecutionSource) (*moomoo.Execution, error) {
	digest, err := executableHash()
	if err != nil {
		return nil, errors.New("Moomoo executable binding unavailable")
	}
	b := &moomoo.Broker{Config: config, Store: store, Source: source,
		Reader: moomoo.NewClient(filepath.Join(dir, "moomoo.token.json")),
		Writer: moomoo.NewTradingClient(filepath.Join(dir, "moomoo-trading.token.json"), config.AccountID)}
	execution := &moomoo.Execution{Broker: b}
	b.Activation = &moomoo.Activation{Dir: dir, Binding: moomoo.ActivationBinding{AccountID: config.AccountID, PolicyID: config.ExecutionPolicy,
		Release: version, ConfigSHA256: moomoo.ConfigDigest(config), ExecutableSHA256: digest}, CheckReady: func() error {
		if configPath != "" {
			current, err := moomoo.LoadConfig(configPath)
			if err != nil || moomoo.ConfigDigest(current) != moomoo.ConfigDigest(config) {
				return errors.New("Moomoo configuration changed; restart and activate the reviewed release")
			}
		}
		currentBinary, err := executableHash()
		if err != nil || currentBinary != digest {
			return errors.New("Moomoo executable identity changed")
		}
		return execution.CheckReady()
	}}
	// A restart always requires fresh native activation. This also resolves any
	// prior activation-write uncertainty under the process's lifetime state lock.
	if err := b.Activation.Pause("Moomoo process started; fresh native activation required"); err != nil {
		return nil, err
	}
	return execution, nil
}

func enableExecution(ctx context.Context, execution *moomoo.Execution) error {
	if execution == nil || execution.Broker == nil || execution.Broker.Activation == nil {
		return errors.New("native execution is not installed")
	}
	if err := execution.Prepare(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return execution.Broker.Activation.Enable()
}
