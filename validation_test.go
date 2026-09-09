// Tests that malformed or hostile input is rejected rather than panicking,
// over-allocating, or escaping the reftable directory.

package reftable

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A manifest entry is joined onto the reftable directory, and filepath.Join
// cleans "..", so an unvalidated name addresses arbitrary paths.
func TestRejectsTraversalInTablesList(t *testing.T) {
	for _, name := range []string{
		"../escape.ref",
		"../../../etc/passwd",
		"sub/dir.ref",
		`sub\dir.ref`,
		"..",
		".",
		"",
	} {
		if err := validateTableName(name); err == nil {
			t.Errorf("validateTableName(%q) = nil, want error", name)
		}
	}
	if err := validateTableName("0x000000000001-0x000000000001-abcdef12.ref"); err != nil {
		t.Errorf("validateTableName(valid) = %v, want nil", err)
	}
}

// End to end: a hostile tables.list must not open, or later remove, a table
// outside the reftable directory.
func TestStackRejectsTraversalManifest(t *testing.T) {
	root := t.TempDir()
	rtDir := filepath.Join(root, "reftable")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{rtDir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	victim := filepath.Join(outside, "victim.ref")
	_, _ = constructTestTable(t, []RefRecord{{RefName: "refs/heads/v", UpdateIndex: 1, Value: testHash(1)}}, nil, Config{})
	if err := os.WriteFile(victim, []byte("not a reftable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rtDir, "tables.list"),
		[]byte("../outside/victim.ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := NewStack(NewLocalStorage(rtDir), Config{})
	if err == nil {
		st.Close()
		t.Fatal("NewStack accepted a manifest entry outside the reftable dir")
	}
	if !errors.Is(err, fmtError) {
		t.Errorf("got %v, want a format error", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("file outside the reftable dir was disturbed: %v", err)
	}
}

// The hash ID is 4 raw bytes from the header and is consumed before the CRC is
// compared, so an unknown value must be an error rather than a panic.
func TestUnknownHashIDIsAnError(t *testing.T) {
	// SHA-256 forces a v2 header, which carries the hash id explicitly.
	_, reader := constructTestTable(t, []RefRecord{
		{RefName: "refs/heads/main", UpdateIndex: 1, Value: make([]byte, 32)},
	}, nil, Config{HashID: SHA256ID})

	raw := append([]byte(nil), reader.src.(*ByteBlockSource).Source...)
	if n := bytes.Count(raw, []byte("s256")); n != 2 {
		t.Fatalf("expected 2 copies of the hash id in the header/footer, got %d", n)
	}
	// Corrupt both copies so the start/tail header comparison still passes and
	// the unknown hash id is what NewReader actually trips over. The CRC is
	// left stale on purpose: this must fail before the CRC is even checked.
	raw = bytes.ReplaceAll(raw, []byte("s256"), []byte("XXXX"))

	if _, err := NewReader(&ByteBlockSource{Source: raw}, "corrupt"); err == nil {
		t.Fatal("NewReader accepted an unknown hash id")
	}
}

func TestNewWriterRejectsBadConfig(t *testing.T) {
	if _, err := NewWriter(&bytes.Buffer{}, &Config{HashID: HashID{'n', 'o', 'p', 'e'}}); err == nil {
		t.Error("NewWriter accepted an unknown hash id")
	}
	// Must be rejected without first allocating a 1GiB block.
	if _, err := NewWriter(&bytes.Buffer{}, &Config{BlockSize: 1 << 30}); err == nil {
		t.Error("NewWriter accepted an oversized block size")
	}
}

func TestNewWriterRejectsSmallBlocks(t *testing.T) {
	for _, hashID := range []HashID{NullHashID, SHA1ID, SHA256ID} {
		version := 1
		if hashID == SHA256ID {
			version = 2
		}
		// The file header, block header, and restart count must all fit.
		minimum := uint32(headerSize(version) + 4 + 2)
		for size := uint32(1); size < minimum; size++ {
			t.Run(fmt.Sprintf("hash=%x/size=%d", hashID, size), func(t *testing.T) {
				if _, err := NewWriter(&bytes.Buffer{}, &Config{HashID: hashID, BlockSize: size}); err == nil {
					t.Fatal("NewWriter accepted a block too small for its headers")
				}
			})
		}
		for _, size := range []uint32{0, minimum} {
			t.Run(fmt.Sprintf("hash=%x/valid-size=%d", hashID, size), func(t *testing.T) {
				w, err := NewWriter(&bytes.Buffer{}, &Config{HashID: hashID, BlockSize: size})
				if err != nil {
					t.Fatal(err)
				}
				w.SetLimits(1, 1)
				err = w.AddRef(&RefRecord{RefName: "refs/heads/a", UpdateIndex: 1, Value: make([]byte, hashID.Size())})
				if size == minimum {
					// Structurally large enough for headers, but not this record.
					if err == nil {
						t.Fatal("expected record-too-large error")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil && !errors.Is(err, ErrEmptyTable) {
					t.Fatal(err)
				}
			})
		}
	}
}

// A log block declares its decompressed size. DEFLATE cannot expand by more
// than 1032:1, so a huge declared size over a tiny block is malformed and must
// not be preallocated.
func TestLogBlockRejectsImpossibleSize(t *testing.T) {
	block := make([]byte, 64)
	block[0] = blockTypeLog
	block[1], block[2], block[3] = 0xff, 0xff, 0xff // sz = 16MiB from a 64 byte block

	if _, err := newBlockReader(block, 0, 64, 20); err == nil {
		t.Fatal("newBlockReader accepted a log block declaring an impossible size")
	}
}
