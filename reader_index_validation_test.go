package reftable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"testing"
)

// Append a one-entry log-index root to an otherwise valid table. Keeping the
// original blocks and recalculating the footer CRC isolates index traversal
// from header validation. Unlike fixed byte flips, the fixture is independent
// of zlib output and changes in the writer's index layout.
func readerWithLogIndexTarget(t *testing.T, original *Reader, key string, target, rootOffset uint64) *Reader {
	t.Helper()
	bw := newBlockWriter(blockTypeIndex, make([]byte, 256), 0, original.hashSize)
	if !bw.add(&indexRecord{LastKey: key, Offset: target}) {
		t.Fatal("index record does not fit in fixture block")
	}
	raw := original.src.(*ByteBlockSource).Source
	data := bytes.Clone(raw[:original.size])
	data = append(data, bw.finish()...)
	footer := bytes.Clone(raw[original.size:])
	// LogIndexOffset is the fifth uint64 following the duplicated header.
	binary.BigEndian.PutUint64(footer[headerSize(original.version)+4*8:], rootOffset)
	binary.BigEndian.PutUint32(footer[len(footer)-4:], crc32.ChecksumIEEE(footer[:len(footer)-4]))
	data = append(data, footer...)
	r, err := NewReader(&ByteBlockSource{Source: data}, "index-target.ref")
	if err != nil {
		t.Fatalf("fixture must pass header/footer validation: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestSeekLogRejectsInvalidIndexTargets(t *testing.T) {
	var refs []RefRecord
	var logs []LogRecord
	for i := range 500 {
		name := fmt.Sprintf("refs/heads/b%06d", i)
		refs = append(refs, RefRecord{RefName: name, UpdateIndex: 1, Value: testHash(i)})
		logs = append(logs, LogRecord{
			RefName: name, UpdateIndex: 1,
			New: testHash(i), Old: testHash(i),
			Name: "n", Email: "e@x", Message: "m",
		})
	}
	_, original := constructTestTable(t, refs, logs, Config{BlockSize: 256})
	t.Cleanup(original.Close)
	want := logs[0]
	logOffset := original.offsets[blockTypeLog].Offset
	if logOffset == 0 || original.offsets[blockTypeLog].IndexOffset == 0 {
		t.Fatal("fixture must contain refs, logs, and a log index")
	}

	for _, tc := range []struct {
		name       string
		target     uint64
		rootOffset uint64
		wantError  bool
	}{
		{"valid", logOffset, original.size, false},
		{"target_past_eof", math.MaxUint64, original.size, true},
		{"target_ref_block", 0, original.size, true},
		{"root_past_eof", logOffset, math.MaxUint64, true},
		{"root_log_block", logOffset, logOffset, true},
		// The fixture block sits at original.size, so pointing its single
		// entry at that same offset makes the index reference itself. Without
		// the maxIndexDepth bound in seekIndexed this descends forever.
		{"cyclic_index", original.size, original.size, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := readerWithLogIndexTarget(t, original, want.key(), tc.target, tc.rootOffset)
			it, err := r.SeekLog(want.RefName, want.UpdateIndex)
			if tc.wantError {
				if !errors.Is(err, fmtError) {
					t.Fatalf("SeekLog error = %v, want format error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got LogRecord
			ok, err := it.NextLog(&got)
			if err != nil || !ok || got.RefName != want.RefName || got.UpdateIndex != want.UpdateIndex || !bytes.Equal(got.New, want.New) {
				t.Fatalf("valid index target: got (%+v, %v, %v)", got, ok, err)
			}
		})
	}
}
