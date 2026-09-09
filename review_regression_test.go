package reftable

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
)

func regressionStack(t *testing.T, storage Storage) *Stack {
	t.Helper()
	st, err := NewStack(storage, Config{})
	if err != nil {
		t.Fatal(err)
	}
	st.disableAutoCompact = true
	t.Cleanup(st.Close)
	return st
}

func regressionAddRef(t *testing.T, st *Stack, name string) {
	t.Helper()
	index := st.NextUpdateIndex()
	err := st.Add(func(w *Writer) error {
		w.SetLimits(index, index)
		return w.AddRef(&RefRecord{
			RefName: name, UpdateIndex: index,
			Value: bytes.Repeat([]byte{1}, 20),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func regressionRequireRef(t *testing.T, tab Table, name string) {
	t.Helper()
	it, err := tab.SeekRef(name)
	if err != nil {
		t.Fatal(err)
	}
	var ref RefRecord
	ok, err := it.NextRef(&ref)
	if err != nil || !ok || ref.RefName != name {
		t.Fatalf("seek %q: got (%+v, %v, %v)", name, ref, ok, err)
	}
}

// Interleave two independent stacks deterministically, without goroutines or
// sleeps. Compaction calls Update after releasing the global manifest lock.
type regressionUpdateHook struct {
	Storage
	hook func()
}

func (s *regressionUpdateHook) Update(name string) (AtomicWriter, error) {
	if s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	return s.Storage.Update(name)
}

func TestRegressionCompactionPreservesConcurrentAddition(t *testing.T) {
	dir := t.TempDir()
	storage := &regressionUpdateHook{Storage: NewLocalStorage(dir)}
	compactor := regressionStack(t, storage)
	regressionAddRef(t, compactor, "refs/heads/a")
	regressionAddRef(t, compactor, "refs/heads/b")
	writer := regressionStack(t, NewLocalStorage(dir))
	interleaved := false
	storage.hook = func() {
		regressionAddRef(t, writer, "refs/heads/c")
		regressionRequireRef(t, writer.Merged(), "refs/heads/c")
		interleaved = true
	}
	if err := compactor.CompactAll(nil); err != nil {
		t.Fatal(err)
	}
	if !interleaved {
		t.Fatal("compaction did not execute the interleaved addition")
	}
	fresh := regressionStack(t, NewLocalStorage(dir))
	for _, name := range []string{"refs/heads/a", "refs/heads/b", "refs/heads/c"} {
		regressionRequireRef(t, fresh.Merged(), name)
	}
}

func TestRegressionCompactionPreservesChangedPrefix(t *testing.T) {
	dir := t.TempDir()
	storage := &regressionUpdateHook{Storage: NewLocalStorage(dir)}
	compactor := regressionStack(t, storage)
	for _, name := range []string{"a", "b", "c", "d"} {
		regressionAddRef(t, compactor, "refs/heads/"+name)
	}
	other := regressionStack(t, NewLocalStorage(dir))
	storage.hook = func() {
		if ok, err := other.compactRange(0, 1, nil); err != nil || !ok {
			t.Fatalf("prefix compaction: %v, %v", ok, err)
		}
	}
	if ok, err := compactor.compactRange(2, 3, nil); err != nil || !ok {
		t.Fatalf("suffix compaction: %v, %v", ok, err)
	}
	fresh := regressionStack(t, NewLocalStorage(dir))
	if len(fresh.stack) != 2 {
		t.Fatalf("got %d tables, want both compacted ranges", len(fresh.stack))
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		regressionRequireRef(t, fresh.Merged(), "refs/heads/"+name)
	}
}

func TestRegressionCompactionLockContention(t *testing.T) {
	dir := t.TempDir()
	storage := &regressionUpdateHook{Storage: NewLocalStorage(dir)}
	st := regressionStack(t, storage)
	regressionAddRef(t, st, "refs/heads/a")
	regressionAddRef(t, st, "refs/heads/b")
	storage.hook = func() {
		lock, err := storage.LockForWrite(listFileName)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { lock.Close() })
	}
	if ok, err := st.compactRange(0, 1, nil); ok || err != nil {
		t.Fatalf("contention: got (%v, %v), want (false, nil)", ok, err)
	}
	lock, err := storage.LockForWrite(listFileName)
	if err == nil {
		lock.Close()
		t.Fatal("compaction removed the competing writer's lock")
	}
	if !os.IsExist(err) {
		t.Fatal(err)
	}
	regressionRequireRef(t, st.Merged(), "refs/heads/a")
	regressionRequireRef(t, st.Merged(), "refs/heads/b")
}

func TestRegressionReloadFailurePreservesExistingReaders(t *testing.T) {
	st := regressionStack(t, NewLocalStorage(t.TempDir()))
	regressionAddRef(t, st, "refs/heads/a")
	regressionRequireRef(t, st.Merged(), "refs/heads/a")
	// The first reader is borrowed from the live stack; the second open fails.
	if err := st.reloadOnce([]string{st.stack[0].Name(), "missing.ref"}, true); err == nil {
		t.Fatal("expected missing-table error")
	}
	regressionRequireRef(t, st.Merged(), "refs/heads/a")
}

func TestRegressionCleanEmptyStack(t *testing.T) {
	st := regressionStack(t, NewLocalStorage(t.TempDir()))
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("empty-stack operation panicked: %v", p)
		}
	}()
	if err := st.Clean(); err != nil {
		t.Fatal(err)
	}
	if st.Merged().MinUpdateIndex() != 0 || st.Merged().MaxUpdateIndex() != 0 {
		t.Fatal("empty stack must have zero update-index bounds")
	}
	if err := st.CompactAll(&LogExpirationConfig{Time: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestRegressionIndexedRefsForUpdateIndex(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, &Config{BlockSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	w.SetLimits(100, 200)
	oid := bytes.Repeat([]byte{1}, 20)
	for i := 0; i < 100; i++ {
		if err := w.AddRef(&RefRecord{
			RefName:     fmt.Sprintf("refs/heads/%04d", i),
			UpdateIndex: 150, Value: oid,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.Stats.ObjStats.Blocks == 0 {
		t.Fatal("fixture must contain an object index")
	}
	r, err := NewReader(&ByteBlockSource{Source: buf.Bytes()}, "indexed.ref")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	it, err := r.RefsFor(oid)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		var ref RefRecord
		ok, err := it.NextRef(&ref)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if ref.UpdateIndex != 150 {
			t.Fatalf("%s: UpdateIndex = %d, want 150", ref.RefName, ref.UpdateIndex)
		}
		count++
	}
	if count != 100 {
		t.Fatalf("got %d references, want 100", count)
	}
}

func TestRegressionCorruptBlockReturnsError(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, &Config{})
	if err != nil {
		t.Fatal(err)
	}
	w.SetLimits(1, 1)
	if err := w.AddRef(&RefRecord{RefName: "refs/heads/a", UpdateIndex: 1, Value: bytes.Repeat([]byte{1}, 20)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, size := range []uint32{0, 1, (1 << 24) - 1} {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("malformed block caused panic instead of error: %v", p)
				}
			}()
			data := bytes.Clone(buf.Bytes())
			// The block header is not covered by the footer CRC.
			putU24(data[headerSize(1)+1:], size)
			r, err := NewReader(&ByteBlockSource{Source: data}, "corrupt.ref")
			if err != nil {
				return // Rejecting corruption at open time is also correct.
			}
			defer r.Close()
			it, err := r.SeekRef("")
			if err == nil {
				var ref RefRecord
				_, err = it.NextRef(&ref)
			}
			if err == nil {
				t.Fatal("malformed block was accepted")
			}
		})
	}
}

// Model Commit's publication/durability boundary: the rename succeeds, but
// directory fsync reports an error. Committed remains true through embedding.
type regressionCommitErrorWriter struct {
	AtomicWriter
	err error
}

func (w *regressionCommitErrorWriter) Commit() error {
	if err := w.AtomicWriter.Commit(); err != nil {
		return err
	}
	return w.err
}

type regressionCommitErrorStorage struct {
	Storage
	err error
}

func (s *regressionCommitErrorStorage) LockForWrite(name string) (AtomicWriter, error) {
	w, err := s.Storage.LockForWrite(name)
	if err != nil {
		return nil, err
	}
	if name == listFileName {
		return &regressionCommitErrorWriter{AtomicWriter: w, err: s.err}, nil
	}
	return w, nil
}

// Fail the table's directory sync, rather than the manifest's. The table
// has been renamed but is not yet reachable through tables.list.
type regressionTableSyncErrorStorage struct {
	Storage
	err error
}

func (s *regressionTableSyncErrorStorage) Update(name string) (AtomicWriter, error) {
	w, err := s.Storage.Update(name)
	if err != nil || s.err == nil {
		return w, err
	}
	return &regressionCommitErrorWriter{AtomicWriter: w, err: s.err}, nil
}

func TestRegressionTableSyncFailureRemovesUnreferencedTable(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			dir := t.TempDir()
			storage := &regressionTableSyncErrorStorage{Storage: NewLocalStorage(dir)}
			st := regressionStack(t, storage)
			if existing {
				regressionAddRef(t, st, "refs/heads/existing")
			}
			before, err := storage.ReadDir()
			if err != nil {
				t.Fatal(err)
			}
			syncErr := errors.New("injected table sync failure after rename")
			storage.err = syncErr
			index := st.NextUpdateIndex()
			err = st.Add(func(w *Writer) error {
				w.SetLimits(index, index)
				return w.AddRef(&RefRecord{RefName: "refs/heads/new", UpdateIndex: index, Value: testHash(1)})
			})
			if !errors.Is(err, syncErr) {
				t.Fatalf("Add error = %v, want injected sync error", err)
			}
			after, err := storage.ReadDir()
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("failed addition left %d directory entries, want %d", len(after), len(before))
			}
			for i := range before {
				if after[i].Name() != before[i].Name() {
					t.Fatalf("directory entry changed: %s -> %s", before[i].Name(), after[i].Name())
				}
			}
			fresh := regressionStack(t, NewLocalStorage(dir))
			if existing {
				regressionRequireRef(t, fresh.Merged(), "refs/heads/existing")
			}
			if ref, err := ReadRef(fresh.Merged(), "refs/heads/new"); err != nil || ref != nil {
				t.Fatalf("failed addition became visible: %+v, %v", ref, err)
			}
			storage.err = nil
			regressionAddRef(t, st, "refs/heads/new")
			regressionRequireRef(t, st.Merged(), "refs/heads/new")
		})
	}
}

func TestRegressionCompactedManifestSurvivesSyncError(t *testing.T) {
	dir := t.TempDir()
	storage := &regressionCommitErrorStorage{Storage: NewLocalStorage(dir)}
	st := regressionStack(t, storage)
	regressionAddRef(t, st, "refs/heads/a")
	regressionAddRef(t, st, "refs/heads/b")
	syncErr := errors.New("injected compaction directory sync failure")
	storage.err = syncErr
	requirePostCommitError(t, st.CompactAll(nil), syncErr)
	fresh := regressionStack(t, NewLocalStorage(dir))
	if len(fresh.stack) != 1 {
		t.Fatalf("got %d tables, want published compacted table", len(fresh.stack))
	}
	regressionRequireRef(t, fresh.Merged(), "refs/heads/a")
	regressionRequireRef(t, fresh.Merged(), "refs/heads/b")
}

func TestRegressionPublishedManifestSurvivesSyncError(t *testing.T) {
	dir := t.TempDir()
	syncErr := errors.New("injected directory sync failure after rename")
	st := regressionStack(t, &regressionCommitErrorStorage{
		Storage: NewLocalStorage(dir), err: syncErr,
	})
	err := st.Add(func(w *Writer) error {
		w.SetLimits(1, 1)
		return w.AddRef(&RefRecord{RefName: "refs/heads/a", UpdateIndex: 1, Value: bytes.Repeat([]byte{1}, 20)})
	})
	requirePostCommitError(t, err, syncErr)
	// The manifest is already visible. Its tables must not be rolled back,
	// even though the caller was told that durability could not be confirmed.
	fresh := regressionStack(t, NewLocalStorage(dir))
	regressionRequireRef(t, fresh.Merged(), "refs/heads/a")
}
