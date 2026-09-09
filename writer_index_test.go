package reftable

import (
	"fmt"
	"testing"
)

// finishSection must flush every index level's final block and must not carry
// index records into the next section. Previously this shape lost reflogs
// through the index with no error reported.
func TestIndexCoversEveryRecord(t *testing.T) {
	for _, blockSize := range []uint32{256, 512, 4096} {
		t.Run(fmt.Sprintf("blockSize=%d", blockSize), func(t *testing.T) {
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

			writer, reader := constructTestTable(t, refs, logs, Config{BlockSize: blockSize})
			if n := len(writer.index); n != 0 {
				t.Errorf("writer kept %d index record(s) after the last section", n)
			}

			for _, r := range refs {
				got, err := ReadRef(reader, r.RefName)
				if err != nil {
					t.Fatalf("ReadRef(%s): %v", r.RefName, err)
				}
				if got == nil {
					t.Fatalf("ref %s unreachable through the index", r.RefName)
				}
			}
			for _, l := range logs {
				got, err := ReadLogAt(reader, l.RefName, 1)
				if err != nil {
					t.Fatalf("ReadLogAt(%s): %v", l.RefName, err)
				}
				if got == nil {
					t.Fatalf("log %s unreachable through the index", l.RefName)
				}
			}
		})
	}
}
