package reftable

import (
	"bytes"
	"compress/zlib"
	"testing"
)

func TestBlockReaderRejectsInvalidLayout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		data      []byte
		headerOff uint32
	}{
		{"missing_header", nil, 0},
		{"short_header", []byte{'r', 0, 0}, 0},
		{"header_offset", []byte{'r', 0, 0, 6, 0, 0}, 24},
		{"oversized_restart_count", []byte{'r', 0, 0, 6, 255, 255}, 0},
		{"restart_in_header", []byte{'r', 0, 0, 10, 0, 0, 0, 0, 0, 1}, 0},
		{"restart_in_trailer", []byte{'r', 0, 0, 10, 0, 0, 0, 5, 0, 1}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newBlockReader(tc.data, tc.headerOff, 0, 20); err == nil {
				t.Fatal("invalid block layout accepted")
			}
		})
	}
}

func TestBlockReaderRejectsOversizedLogStream(t *testing.T) {
	var compressed bytes.Buffer
	compressed.Write([]byte{'g', 0, 0, 6}) // Declares only two payload bytes.
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(bytes.Repeat([]byte{0}, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newBlockReader(compressed.Bytes(), 0, 0, 20); err == nil {
		t.Fatal("oversized decompressed payload accepted")
	}
}
