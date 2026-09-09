/*
Copyright 2020 Google LLC

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/

package reftable

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

// CompactionStats holds some statistics of compaction over the
// lifetime of the stack.
type CompactionStats struct {
	Bytes uint64

	// All entries written, including from failed compaction attempts.
	EntriesWritten uint64
	Attempts       int
	Failures       int
}

// Stack is an auto-compacting stack of reftables.
type Stack struct {
	storage Storage
	cfg     Config

	// mutable
	stack              []*Reader
	merged             *Merged
	disableAutoCompact bool

	Stats CompactionStats
}

const listFileName = "tables.list"

// NewStack returns a new stack.
func NewStack(storage Storage, cfg Config) (*Stack, error) {
	if cfg.HashID == NullHashID {
		cfg.HashID = SHA1ID
	}
	switch cfg.HashID {
	case SHA1ID, SHA256ID:
	default:
		return nil, fmt.Errorf("reftable: unknown hash ID %q", cfg.HashID)
	}

	st := &Stack{
		storage: storage,
		cfg:     cfg,
	}

	if err := st.reload(true); err != nil {
		return nil, err
	}

	return st, nil
}

func (st *Stack) String() string {
	var nms []string
	for _, r := range st.stack {
		nms = append(nms, r.Name())
	}
	return fmt.Sprintf("%v", nms)
}

// validateTableName rejects manifest entries that would escape the reftable
// directory. Storage implementations join these names onto a base directory,
// and filepath.Join cleans "..", so an unvalidated name from tables.list can
// address — and Remove — arbitrary files. Table names are always plain
// filenames, so refusing separators and dot components is sufficient.
func validateTableName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("%w: invalid table name %q", fmtError, name)
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "\x00") {
		return fmt.Errorf("%w: table name %q must be a plain filename", fmtError, name)
	}
	return nil
}

func (st *Stack) readNames() ([]string, error) {
	bs, err := st.storage.OpenBlockSource(listFileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer bs.Close()

	data, err := bs.ReadBlock(0, int(bs.Size()))
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte("\n"))

	var res []string
	for _, l := range lines {
		if len(l) == 0 {
			continue
		}
		name := string(l)
		if err := validateTableName(name); err != nil {
			return nil, err
		}
		res = append(res, name)
	}

	return res, nil
}

// Returns the merged stack. The stack is only valid until the next
// write, as writes may trigger reloads
func (st *Stack) Merged() *Merged {
	return st.merged
}

// Close releases file descriptors associated with this stack.
func (st *Stack) Close() {
	// Read the file list again, for closing files that were opened on windows.
	names, err := st.readNames()
	if err != nil {
		// On error, we won't remove anything.
		names = nil
	}
	nameSet := make(map[string]struct{}, len(names))
	for _, nm := range names {
		nameSet[nm] = struct{}{}
	}
	for _, r := range st.stack {
		r.Close()
		if _, ok := nameSet[r.Name()]; len(nameSet) > 0 && !ok {
			st.storage.Remove(r.Name())
		}
	}
	st.stack = nil
}

func (st *Stack) reloadOnce(names []string, reuseOpen bool) error {
	cur := map[string]*Reader{}

	for _, r := range st.stack {
		cur[r.Name()] = r
	}

	var newTables, opened []*Reader
	retained := make(map[string]bool, len(names))
	defer func() {
		for _, t := range opened {
			t.Close()
		}
	}()

	for _, name := range names {
		retained[name] = true
		rd := cur[name]
		if reuseOpen && rd != nil {
			delete(cur, name)
		} else {
			bs, err := st.storage.OpenBlockSource(name)
			if err != nil {
				return err
			}

			rd, err = NewReader(bs, name)
			if err != nil {
				bs.Close()
				return fmt.Errorf("NewReader(%s): %w", name, err)
			}
			opened = append(opened, rd)
		}
		newTables = append(newTables, rd)
	}

	var tabs []Table
	for _, r := range newTables {
		tabs = append(tabs, r)
	}
	merged, err := NewMerged(tabs, st.cfg.HashID)
	if err != nil {
		return err
	}
	merged.suppressDeletions = true

	// Only transfer ownership once the entire replacement is valid.
	st.stack = newTables
	st.merged = merged
	opened = nil
	for _, old := range cur {
		old.Close()

		// On windows, we may only be able to close after
		// closing file handles.
		if !retained[old.Name()] {
			st.storage.Remove(old.Name())
		}
	}
	return nil
}

// TODO: reload does not cache the (st_dev, st_ino) of tables.list,
// so it cannot distinguish "tables.list unchanged" from "tables.list
// was replaced by a file with the same inode after the inode was
// recycled". The C version (stack.c) caches list_st across reloads
// to defeat this ABA race. UpToDate has the same exposure.
func (st *Stack) reload(reuseOpen bool) error {
	var delay time.Duration
	deadline := time.Now().Add(5 * time.Second / 2)
	for time.Now().Before(deadline) {
		names, err := st.readNames()
		if err != nil {
			return err
		}
		err = st.reloadOnce(names, reuseOpen)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		after, err := st.readNames()
		if err != nil {
			return err
		}
		if reflect.DeepEqual(after, names) {
			// XXX: propogate name
			return os.ErrNotExist
		}

		// compaction changed names; back off and retry.
		delay = 2*delay + time.Millisecond*time.Duration(1+rand.IntN(2))
		time.Sleep(delay)
	}

	return ErrReloadTimeout
}

// ErrLockFailure means a write could not proceed before publishing its
// manifest, for example because a lock was contended or the stack was stale.
// Callers may retry the transaction when errors.Is(err, ErrLockFailure).
// Errors after publication never match this sentinel.
var ErrLockFailure = errors.New("reftable: lock failure")

// ErrReloadTimeout means no consistent stack snapshot could be loaded before
// the reload deadline. It does not mean that a preceding write was aborted.
var ErrReloadTimeout = errors.New("reftable: stack reload timed out")

// ErrPostCommit means the manifest was published, but durability confirmation,
// reloading, or maintenance failed afterward. Do not replay the transaction.
// Use errors.As to inspect the Cause of the accompanying *PostCommitError.
var ErrPostCommit = errors.New("reftable: error after manifest publication")

// PostCommitError reports a failure after a manifest was published. The update
// is visible, though durability may be uncertain and the Stack may still hold
// its previous snapshot. Reopen the stack to inspect the current state; do not
// blindly retry the write.
//
// Cause is deliberately not part of the Unwrap chain: a lock error during
// post-commit maintenance must not classify a committed write as retryable.
// Callers can inspect it explicitly with errors.Is or errors.As after checking
// the publication status.
type PostCommitError struct {
	Cause error
}

func (e *PostCommitError) Error() string {
	return fmt.Sprintf("%v: %v", ErrPostCommit, e.Cause)
}

func (e *PostCommitError) Unwrap() error { return ErrPostCommit }

func postCommitError(err error) error {
	if err == nil {
		return nil
	}
	// errors.As, not a type assertion: callers reach here through
	// errors.Join and fmt.Errorf("%w", ...), so the PostCommitError is
	// frequently wrapped rather than the top-level value. A bare assertion
	// misses those and wraps a second time, and the whole point of this type
	// is that a committed write is not reclassified as retryable.
	var pce *PostCommitError
	if errors.As(err, &pce) {
		return err
	}
	return &PostCommitError{Cause: err}
}

func (st *Stack) UpToDate() (bool, error) {
	names, err := st.readNames()
	if err != nil {
		return false, err
	}

	if len(names) != len(st.stack) {
		return false, nil
	}

	for i, e := range st.stack {
		if e.name != names[i] {
			return false, nil
		}
	}
	return true, nil
}

// Add a new reftable to stack, transactionally. ErrPostCommit means the update
// was published but a later step failed; the callback must not be replayed.
func (st *Stack) Add(write func(w *Writer) error) error {
	published, err := st.add(write)
	if err != nil {
		if errors.Is(err, ErrLockFailure) {
			st.reload(true)
		}
		return err
	}

	if !st.disableAutoCompact {
		err = st.AutoCompact()
		if published {
			// The addition committed, even if maintenance cannot lock or
			// reload the stack. Do not advertise a retryable write failure.
			return postCommitError(err)
		}
		return err
	}
	return nil
}

func (st *Stack) add(write func(w *Writer) error) (bool, error) {
	tr, err := st.NewAddition()
	if err != nil {
		return false, err
	}
	defer tr.Close()
	if err := tr.Add(write); err != nil {
		return false, err
	}

	err = tr.Commit()
	return tr.lockFile.Committed(), err
}

// Addition is a transaction that adds new tables to the top of the
// stack.
type Addition struct {
	lockFile        AtomicWriter
	stack           *Stack
	names           []string
	newTables       []string
	nextUpdateIndex uint64
}

// NewAddition returns an Addition instance. As a side effect, this
// takes a global filesystem lock on the ref database.
func (st *Stack) NewAddition() (*Addition, error) {
	tr := Addition{
		stack: st,
	}
	var err error
	tr.lockFile, err = st.storage.LockForWrite(listFileName)
	if errors.Is(err, os.ErrExist) {
		return nil, ErrLockFailure
	}
	if err != nil {
		return nil, err
	}
	for _, e := range st.stack {
		tr.names = append(tr.names, e.name)
	}
	if ok, err := tr.stack.UpToDate(); err != nil {
		tr.Close()
		return nil, err
	} else if !ok {
		tr.Close()
		return nil, ErrLockFailure
	}
	tr.nextUpdateIndex = tr.stack.NextUpdateIndex()
	return &tr, nil
}

// Add calls the given function to write a new table at the top of
// the stack.
func (tr *Addition) Add(write func(w *Writer) error) error {
	dest := formatName(tr.nextUpdateIndex, tr.nextUpdateIndex) + ".ref"

	tab, err := tr.stack.storage.Update(dest)
	if err != nil {
		return err
	}
	defer func() {
		if tab != nil {
			tab.Close()
		}
	}()

	wr, err := NewWriter(tab, &tr.stack.cfg)
	if err != nil {
		return err
	}

	if err := write(wr); err != nil {
		return err
	}

	if err := wr.Close(); err != nil {
		if errors.Is(err, ErrEmptyTable) {
			return nil
		}
		return err
	}

	if wr.minUpdateIndex < tr.nextUpdateIndex {
		return ErrLockFailure
	}

	if err := tr.stack.checkAddition(filepath.Base(tab.Name())); err != nil {
		return err
	}

	if err := tab.Commit(); err != nil {
		// The table rename may have succeeded before directory sync failed.
		// It is not in the manifest yet, so remove it explicitly: Close must
		// preserve published files, and this one is not tracked by tr yet.
		if tab.Committed() {
			return errors.Join(err, tr.stack.storage.Remove(dest))
		}
		return err
	}

	tr.names = append(tr.names, dest)
	tr.newTables = append(tr.newTables, dest)
	tr.nextUpdateIndex = wr.maxUpdateIndex + 1
	return nil
}

// Close releases all non-committed data from the transaction.
func (tr *Addition) Close() {
	for _, nm := range tr.newTables {
		tr.stack.storage.Remove(nm)
	}
	tr.newTables = nil
	tr.lockFile.Close()
}

// Commit commits the changes to the database, releasing the lock. It returns
// ErrPostCommit if the manifest was published but a subsequent step failed.
func (tr *Addition) Commit() error {
	if len(tr.newTables) == 0 {
		// Nothing to be done.
		return nil
	}

	if _, err := tr.lockFile.Write([]byte(strings.Join(tr.names, "\n"))); err != nil {
		tr.Close()
		return err
	}

	err := tr.lockFile.Commit()
	if err != nil && !tr.lockFile.Committed() {
		tr.Close()
		return err
	}
	// Rename may succeed even when the following directory sync fails.
	// Published tables belong to the manifest and must survive cleanup.
	tr.newTables = nil
	return postCommitError(errors.Join(err, tr.stack.reload(true)))
}

func (s *Stack) checkAddition(tabname string) error {
	if s.cfg.SkipNameCheck {
		return nil
	}
	bs, err := s.storage.OpenBlockSource(tabname)
	if err != nil {
		return err
	}
	r, err := NewReader(bs, tabname)
	if err != nil {
		bs.Close()
		return fmt.Errorf("NewReader(%s): %w", tabname, err)
	}
	defer r.Close()
	it, err := r.SeekRef("")
	if err != nil {
		return err
	}

	var recs []RefRecord
	for {
		var rec RefRecord
		ok, err := it.NextRef(&rec)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		recs = append(recs, rec)
	}

	return validateRefRecordAddition(s.Merged(), recs)
}

// formatName builds the filename for a table covering [min, max]. The random
// suffix keeps names unique when several tables cover the same update-index
// range. rand/v2's top-level source is safe for concurrent use, which matters
// because one process commonly holds a Stack per repository and writes to them
// from different goroutines; the old package-level *rand.Rand was not, and
// torn reads produced duplicate names.
func formatName(min, max uint64) string {
	return fmt.Sprintf("0x%012x-0x%012x-%08x", min, max, rand.Uint32())
}

// NextUpdateIndex returns the update index at which to write the next table.
func (st *Stack) NextUpdateIndex() uint64 {
	if sz := len(st.stack); sz > 0 {
		return st.stack[sz-1].MaxUpdateIndex() + 1
	}
	return 1
}

// compactLocked writes the compacted version of tables [first,last]
// into a temporary file, whose name is returned.
func (st *Stack) compactLocked(first, last int, expiration *LogExpirationConfig) (AtomicWriter, error) {
	fn := formatName(st.stack[first].MinUpdateIndex(),
		st.stack[last].MaxUpdateIndex()) + ".ref"

	tmpTable, err := st.storage.Update(fn)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tmpTable != nil {
			tmpTable.Close()
		}
	}()

	wr, err := NewWriter(tmpTable, &st.cfg)
	if err != nil {
		return nil, err
	}

	if err := st.writeCompact(wr, first, last, expiration); err != nil {
		return nil, err
	}

	if err := wr.Close(); err != nil {
		return nil, err
	}

	result := tmpTable
	tmpTable = nil
	return result, nil
}

func (st *Stack) writeCompact(wr *Writer, first, last int, expiration *LogExpirationConfig) error {
	// do it.
	wr.SetLimits(st.stack[first].MinUpdateIndex(),
		st.stack[last].MaxUpdateIndex())

	var subtabs []Table
	for i := first; i <= last; i++ {
		subtabs = append(subtabs, st.stack[i])
	}

	merged, err := NewMerged(subtabs, st.cfg.HashID)
	if err != nil {
		return err
	}
	it, err := merged.SeekRef("")
	if err != nil {
		return err
	}

	var entries uint64
	for {
		var rec RefRecord
		ok, err := it.NextRef(&rec)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		if first == 0 && rec.IsDeletion() {
			continue
		}

		if err := wr.AddRef(&rec); err != nil {
			return err
		}
		entries++
	}

	it, err = merged.SeekLog("", math.MaxUint64)
	if err != nil {
		return err
	}
	for {
		var rec LogRecord
		ok, err := it.NextLog(&rec)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		if expiration != nil {
			if expiration.Time > 0 && rec.Time < expiration.Time {
				continue
			}

			if expiration.MaxUpdateIndex != 0 && rec.UpdateIndex > expiration.MaxUpdateIndex {
				continue
			}
			if expiration.MinUpdateIndex != 0 && rec.UpdateIndex < expiration.MinUpdateIndex {
				continue
			}
		}

		if err := wr.AddLog(&rec); err != nil {
			return err
		}
		entries++
	}

	st.Stats.EntriesWritten += entries
	return nil
}

func (st *Stack) compactRangeStats(first, last int, expiration *LogExpirationConfig) (bool, error) {
	ok, err := st.compactRange(first, last, expiration)
	if !ok {
		st.Stats.Failures++
	}
	return ok, err
}

func (st *Stack) compactRange(first, last int, expiration *LogExpirationConfig) (bool, error) {
	if first > last || (first == last && expiration == nil) {
		return true, nil
	}
	st.Stats.Attempts++

	lock, err := st.storage.LockForWrite(listFileName)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	defer lock.Close()

	if ok, err := st.UpToDate(); !ok || err != nil {
		return false, err
	}

	var deleteOnSuccess []string
	var subtableLocks []AtomicWriter
	defer func() {
		for _, l := range subtableLocks {
			l.Close()
		}
	}()
	for i := first; i <= last; i++ {
		subtab := st.stack[i].name
		subtabLock, err := st.storage.LockForWrite(subtab)

		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		subtableLocks = append(subtableLocks, subtabLock)
		deleteOnSuccess = append(deleteOnSuccess, subtab)
	}

	lock.Close()

	tmpTable, err := st.compactLocked(first, last, expiration)

	// Compaction + tombstones can create an empty table out of non-empty tables.
	if errors.Is(err, ErrEmptyTable) {
		// In this case, we may have tmpTable == nil
		err = nil
	}

	if err != nil {
		return false, err
	}
	published := false
	if tmpTable != nil {
		defer func() {
			if !published && tmpTable.Committed() {
				st.storage.Remove(tmpTable.Name())
			}
			tmpTable.Close()
		}()
	}

	lock, err = st.storage.LockForWrite(listFileName)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer lock.Close()

	// Other writers can append or compact unrelated tables while the global
	// lock is released. Replace only our still-contiguous range in the latest
	// manifest, preserving every change outside it.
	current, err := st.readNames()
	if err != nil {
		return false, err
	}
	if len(deleteOnSuccess) == 0 {
		// Guaranteed by the first > last check above; keep the slice
		// access below honest if that guard is ever relaxed.
		return false, nil
	}
	start := slices.Index(current, deleteOnSuccess[0])
	end := start + len(deleteOnSuccess)
	if start < 0 || end > len(current) || !slices.Equal(current[start:end], deleteOnSuccess) {
		return false, nil
	}
	var names []string
	names = append(names, current[:start]...)
	if tmpTable != nil {
		if err := tmpTable.Commit(); err != nil {
			return false, err
		}
		names = append(names, tmpTable.Name())
	}
	names = append(names, current[end:]...)

	if _, err := lock.Write([]byte(strings.Join(names, "\n"))); err != nil {
		return false, err
	}
	err = lock.Commit()
	published = err == nil || lock.Committed()
	if !published {
		return false, err
	}

	// Reload closes and removes superseded tables through Storage. A sync
	// failure after publication must not roll back the replacement table.
	reloadErr := st.reload(expiration == nil)
	if reloadErr != nil {
		// reloadOnce never reached its cleanup, so the tables we just
		// replaced are unreferenced by the manifest but still on disk.
		// Nothing else collects them; drop them here.
		for _, nm := range deleteOnSuccess {
			if tmpTable != nil && nm == tmpTable.Name() {
				// Reflog expiry can reuse the name we just published.
				continue
			}
			st.storage.Remove(nm)
		}
	}
	return true, postCommitError(errors.Join(err, reloadErr))
}

func (st *Stack) tableSizesForCompaction() []uint64 {
	var res []uint64
	version := 1
	if st.cfg.HashID == SHA256ID {
		version = 2
	}
	var overhead = uint64(headerSize(version) - 1)
	for _, t := range st.stack {
		res = append(res, t.size-overhead)
	}
	return res
}

type segment struct {
	start int
	end   int // exclusive
	log   int
	bytes uint64
}

func (st *segment) size() int { return st.end - st.start }

func log2(sz uint64) int {
	base := uint64(2)
	if sz == 0 {
		return 0
	}

	l := 0
	for sz > 0 {
		l++
		sz /= base
	}

	return l - 1
}

func sizesToSegments(sizes []uint64) []segment {
	var cur segment
	var res []segment
	for i, sz := range sizes {
		l := log2(sz)
		if cur.log != l && cur.bytes > 0 {
			res = append(res, cur)
			cur = segment{
				start: i,
			}
		}
		cur.log = l
		cur.end = i + 1
		cur.bytes += sz
	}

	res = append(res, cur)
	return res
}

/*
We play the game of 2048: consecutive tables of the same size (as
determined by their log2) are compacted together. We try to combine
the result with preceding tables, if they are smaller (as determined
by their log2). As a result, if we have N entries, each entry will
go into a bigger table in a maximum of log2(N) times, making for
log2(N) * N overall cost.
*/
func suggestCompactionSegment(sizes []uint64) *segment {
	segs := sizesToSegments(sizes)

	minSeg := segment{log: 64}
	for _, st := range segs {
		if st.size() == 1 {
			continue
		}

		if st.log < minSeg.log {
			minSeg = st
		}
	}
	if minSeg.size() == 0 {
		return nil
	}

	for minSeg.start > 0 {
		prev := minSeg.start - 1
		if log2(minSeg.bytes) < log2(sizes[prev]) {
			break
		}

		minSeg.start = prev
		minSeg.bytes += sizes[prev]
	}

	return &minSeg
}

// AutoCompact runs a compaction if the stack looks imbalanced.
func (st *Stack) AutoCompact() error {
	sizes := st.tableSizesForCompaction()
	seg := suggestCompactionSegment(sizes)
	if seg != nil {
		_, err := st.compactRangeStats(seg.start, seg.end-1, nil)
		return err
	}
	return nil
}

// CompactAll compacts the entire stack. If expiration is given, expire log entries.
func (st *Stack) CompactAll(expiration *LogExpirationConfig) error {
	_, err := st.compactRange(0, len(st.stack)-1, expiration)
	return err
}

// Clean removes stale *.ref files. It is only required to be called
// on Windows, if previous processes did not call Stack.Close on exit.
func (st *Stack) Clean() error {
	// Take a lock to prevent concurrent updates.
	add, err := st.NewAddition()
	if err != nil {
		return err
	}
	defer add.Close()

	if err := st.reload(true); err != nil {
		return err
	}

	names := map[string]struct{}{}
	for _, r := range st.stack {
		names[r.Name()] = struct{}{}
	}
	entries, err := st.storage.ReadDir()
	if err != nil {
		return err
	}

	max := st.merged.MaxUpdateIndex()
	for _, e := range entries {
		name := e.Name()
		if _, ok := names[name]; ok {
			continue
		}
		if !strings.HasSuffix(name, ".ref") {
			continue
		}

		bs, err := st.storage.OpenBlockSource(name)
		if err != nil {
			return err
		}

		rd, err := NewReader(bs, name)
		if err != nil {
			bs.Close()
			return fmt.Errorf("NewReader(%s): %w", name, err)
		}

		cur := rd.MaxUpdateIndex()
		rd.Close()
		if cur <= max {
			st.storage.Remove(name)
		}
	}
	return nil
}
