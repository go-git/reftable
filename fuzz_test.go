package reftable

import (
	"bytes"
	"fmt"
	"testing"
)

// corpusTable writes a table with enough refs and reflogs to build a
// multi-level index for every section, which is where the offsets that a
// reader follows out of the file live.
func corpusTable(t testing.TB, refs, logs int, cfg Config) []byte {
	t.Helper()

	hashSize := 20
	if cfg.HashID == SHA256ID {
		hashSize = 32
	}
	hash := func(i int) []byte {
		h := make([]byte, hashSize)
		h[hashSize-1], h[hashSize-2] = byte(i), byte(i>>8)
		return h
	}

	buf := &bytes.Buffer{}
	w, err := NewWriter(buf, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	w.SetLimits(1, 1)
	for i := range refs {
		if err := w.AddRef(&RefRecord{
			RefName: fmt.Sprintf("refs/heads/b%06d", i), UpdateIndex: 1, Value: hash(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range logs {
		if err := w.AddLog(&LogRecord{
			RefName: fmt.Sprintf("refs/heads/b%06d", i), UpdateIndex: 1,
			New: hash(i), Old: hash(i), Name: "n", Email: "e@x", Message: "m",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// driveReader exercises every path that follows an offset, length, or block
// type read out of the table. It must never panic, whatever the bytes say.
func driveReader(data []byte) {
	rd, err := NewReader(&ByteBlockSource{Source: data}, "fuzz")
	if err != nil {
		return
	}
	_ = rd.MaxUpdateIndex()
	_ = rd.MinUpdateIndex()

	for _, key := range []string{"", "refs/heads/b000000", "refs/heads/b000250", "refs/heads/zzz"} {
		if it, err := rd.SeekRef(key); err == nil && it != nil {
			for range 4 {
				var r RefRecord
				if ok, err := it.NextRef(&r); !ok || err != nil {
					break
				}
			}
		}
		if it, err := rd.SeekLog(key, 1); err == nil && it != nil {
			for range 4 {
				var l LogRecord
				if ok, err := it.NextLog(&l); !ok || err != nil {
					break
				}
			}
		}
	}

	// Both hash sizes: objectIDLen comes from the footer and need not match.
	for _, n := range []int{20, 32} {
		oid := make([]byte, n)
		oid[n-1] = 0xfa
		if it, err := rd.RefsFor(oid); err == nil && it != nil {
			var r RefRecord
			it.NextRef(&r)
		}
	}
}

// A corrupt table must produce an error, never a panic. Every byte of a
// reftable is attacker-controlled: it can arrive from a clone, a hostile
// archive, or a partially written file after a crash.
//
// This is a deterministic sweep so it runs in CI; FuzzReader below explores
// the same surface without a fixed stride.
func TestReaderSurvivesCorruption(t *testing.T) {
	shapes := []struct {
		name       string
		refs, logs int
		cfg        Config
	}{
		{"sha1/bs256", 500, 500, Config{BlockSize: 256}},
		{"sha1/unaligned", 300, 300, Config{BlockSize: 256, Unaligned: true}},
		{"sha256/bs256", 100, 100, Config{BlockSize: 256, HashID: SHA256ID}},
	}

	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			base := corpusTable(t, s.refs, s.logs, s.cfg)

			// Stride keeps this fast while still covering every region:
			// headers, block trailers, restart tables and index offsets.
			const stride = 3
			for pos := 0; pos < len(base); pos += stride {
				for _, v := range []byte{0x00, 0x80, 0xff} {
					if base[pos] == v {
						continue
					}
					mutated := append([]byte(nil), base...)
					mutated[pos] = v

					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Fatalf("panic on byte %d = %#02x: %v", pos, v, r)
							}
						}()
						driveReader(mutated)
					}()
				}
			}

			// Truncation at every length must also be handled.
			for n := 0; n < len(base); n += 17 {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic on truncation to %d bytes: %v", n, r)
						}
					}()
					driveReader(base[:n])
				}()
			}
		})
	}
}

func FuzzReader(f *testing.F) {
	f.Add(corpusTable(f, 500, 500, Config{BlockSize: 256}))
	f.Add(corpusTable(f, 300, 300, Config{BlockSize: 256, Unaligned: true}))
	f.Add(corpusTable(f, 100, 100, Config{BlockSize: 256, HashID: SHA256ID}))
	f.Add(corpusTable(f, 60, 60, Config{}))
	f.Add([]byte{})
	f.Add(make([]byte, 1024))

	f.Fuzz(func(t *testing.T, data []byte) {
		driveReader(data)
	})
}
