package researchselection

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFrozenEvidenceReadLimitsAndNoSymlinks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.json")
	content := []byte("{\"synthetic_source\":true}\n")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readRegular(file, 100); err != nil || !bytes.Equal(got, content) {
		t.Fatal("regular source bytes changed", err)
	}
	if _, err := readRegular(file, int64(len(content)-1)); err == nil {
		t.Fatal("source exceeding its bound was read")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(link, 100); err == nil {
		t.Fatal("symlink to an otherwise valid bounded source was accepted")
	}
	if _, err := readRegular(dir, 100); err == nil {
		t.Fatal("non-regular source was accepted")
	}
}
