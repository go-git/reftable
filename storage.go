package reftable

import (
	"cmp"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// AtomicWriter is an abstraction for a {write to temp, close, rename}
// file sink. Close aborts unpublished output and must be idempotent: a
// second Close must not remove a path it no longer owns, because callers
// legitimately Close the same writer twice on error paths.
type AtomicWriter interface {
	io.WriteCloser

	// Name returns the basename this writer's output currently occupies:
	// the temporary name before Commit, the final name after a successful
	// one. It is stable across Close, so `Remove(w.Name())` after an
	// aborted Close never addresses the final path.
	Name() string
	Commit() error

	// Committed reports whether the new file has been published, i.e. the
	// rename succeeded, even if Commit then returned a durability error.
	// Close must not remove a published file. Note this is narrower than
	// "the writer was closed": an aborted writer reports false.
	Committed() bool
}

// Storage is the interface for reading and writing a set of files in
// a flat namespace (ie. single directory)
type Storage interface {
	// Like Update(), but use a ".lock" name to block other
	// writers from writing the same file.  Release the lock by
	// calling Close().
	LockForWrite(name string) (AtomicWriter, error)

	// Prepare for updating the given name. The update becomes
	// visible after calling AtomicWriter.Commit
	Update(name string) (AtomicWriter, error)
	ReadDir() ([]fs.DirEntry, error)
	Remove(name string) error
	OpenBlockSource(name string) (BlockSource, error)
}

type fileWriter struct {
	finalName string
	tempName  string
	committed bool
	aborted   bool
	*os.File
}

func (fw *fileWriter) Committed() bool {
	return fw.committed
}

func (fw *fileWriter) Name() string {
	if !fw.committed {
		return filepath.Base(fw.tempName)
	}

	return filepath.Base(fw.finalName)
}

func (fw *fileWriter) Close() error {
	var closeErr, removeErr error
	if fw.File != nil {
		closeErr = fw.File.Close()
		fw.File = nil
	}
	// Only the first Close of an unpublished writer owns the temp file. A
	// second Close must not unlink the path again: another writer may have
	// created it in the meantime, and removing it would steal their lock.
	if !fw.committed && !fw.aborted {
		fw.aborted = true
		removeErr = os.Remove(fw.tempName)
	}
	return cmp.Or(closeErr, removeErr)
}

// fsyncDir flushes the directory entry for path.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (fw *fileWriter) Commit() error {
	if fw.File == nil {
		return os.ErrClosed
	}
	if err := fw.File.Sync(); err != nil {
		return err
	}
	err := fw.File.Close()
	fw.File = nil
	if err != nil {
		return err
	}
	if err := os.Rename(fw.tempName, fw.finalName); err != nil {
		return err
	}
	fw.committed = true
	return fsyncDir(filepath.Dir(fw.finalName))
}

func newLockForWrite(path string) (AtomicWriter, error) {
	f, err := os.OpenFile(path+".lock", os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &fileWriter{File: f, finalName: path, tempName: f.Name()}, nil
}

func newAtomicWriter(path string) (AtomicWriter, error) {
	name := filepath.Base(path)
	f, err := os.CreateTemp(filepath.Dir(path), name+".*.tmp")
	if err != nil {
		return nil, err
	}
	return &fileWriter{File: f, finalName: path, tempName: f.Name()}, nil
}

func NewLocalStorage(dir string) *localStorage {
	return &localStorage{dir}
}

func (s *localStorage) LockForWrite(name string) (AtomicWriter, error) {
	return newLockForWrite(filepath.Join(s.reftableDir, name))
}

type localStorage struct {
	reftableDir string
}

func (s *localStorage) Update(name string) (AtomicWriter, error) {
	return newAtomicWriter(filepath.Join(s.reftableDir, name))
}

func (s *localStorage) ReadDir() ([]fs.DirEntry, error) {
	return os.ReadDir(s.reftableDir)
}

func (s *localStorage) Sync() error {
	return fsyncDir(s.reftableDir)
}

func (s *localStorage) Remove(name string) error {
	return os.Remove(filepath.Join(s.reftableDir, name))
}

type fileBlockSource struct {
	f interface {
		io.ReaderAt
		io.Closer
	}
	sz uint64
}

// OpenBlockSource opens a file on local disk as a BlockSource.
func (s *localStorage) OpenBlockSource(name string) (BlockSource, error) {
	f, err := os.Open(filepath.Join(s.reftableDir, name))
	if err != nil {
		return nil, fmt.Errorf("NewFileBlockSource: %w", err)
	}

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	return &fileBlockSource{f, uint64(fi.Size())}, nil
}

func (bs *fileBlockSource) Size() uint64 {
	return bs.sz
}

func (bs *fileBlockSource) ReadBlock(off uint64, size int) ([]byte, error) {
	if off > bs.sz {
		return nil, io.EOF
	}
	if off+uint64(size) > bs.sz {
		size = int(bs.sz - off)
	}
	b := make([]byte, size)
	n, err := bs.f.ReadAt(b, int64(off))
	if err != nil {
		return nil, err
	}

	return b[:n], nil
}

func (bs *fileBlockSource) Close() error {
	return bs.f.Close()
}
