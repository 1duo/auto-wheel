package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	aruntime "autowheel/internal/runtime"
)

const operatorTokenFile = "operator-token.json"

// The journal lock must be held when provisioning the installation credential.
// Existing unreadable or malformed credentials are never replaced implicitly.
func ensureOperatorToken(dir string) error {
	_, err := readOperatorToken(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	return aruntime.AtomicJSON(filepath.Join(dir, operatorTokenFile), hex.EncodeToString(secret[:]))
}

func readOperatorToken(dir string) (string, error) {
	f, err := aruntime.PrivateRead(filepath.Join(dir, operatorTokenFile))
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 4096 {
		return "", errors.New("invalid operator credential file")
	}
	var token string
	d := json.NewDecoder(io.LimitReader(f, 4097))
	if d.Decode(&token) != nil || len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
		return "", errors.New("invalid operator credential")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return "", errors.New("invalid operator credential content")
	}
	return token, nil
}

func nativeControlRequest(r *http.Request, dir string) bool {
	if r.Header.Get("Origin") != "" || r.Header.Get("X-AutoWheel-Control") != "confirm" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return false
	}
	// Re-read on each mutation so removing, replacing, or restricting the token
	// immediately revokes its use. Never return the credential through status.
	token, err := readOperatorToken(dir)
	return err == nil && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}
