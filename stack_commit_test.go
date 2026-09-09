package reftable

import (
	"errors"
	"fmt"
	"testing"
)

func requirePostCommitError(t *testing.T, err, cause error) {
	t.Helper()
	if !errors.Is(err, ErrPostCommit) {
		t.Fatalf("got %v, want ErrPostCommit", err)
	}
	if errors.Is(err, ErrLockFailure) {
		t.Fatalf("published update was classified as retryable: %v", err)
	}
	var published *PostCommitError
	if !errors.As(err, &published) || !errors.Is(published.Cause, cause) {
		t.Fatalf("got %v, want explicit post-commit cause %v", err, cause)
	}
}

// Inject reload or maintenance failures only after the real manifest rename.
// No sleep is needed to model a reload exhausting its retry deadline.
type publicationFailureStorage struct {
	Storage
	afterPublication func()
	readErr          error
	manifestLocks    int
	failLockAt       int
}

func (s *publicationFailureStorage) OpenBlockSource(name string) (BlockSource, error) {
	if name == listFileName && s.readErr != nil {
		return nil, s.readErr
	}
	return s.Storage.OpenBlockSource(name)
}

func (s *publicationFailureStorage) LockForWrite(name string) (AtomicWriter, error) {
	if name == listFileName {
		s.manifestLocks++
		if s.manifestLocks == s.failLockAt {
			return nil, fmt.Errorf("injected maintenance failure: %w", ErrLockFailure)
		}
	}
	w, err := s.Storage.LockForWrite(name)
	if err != nil || name != listFileName || s.afterPublication == nil {
		return w, err
	}
	return &publicationHookWriter{AtomicWriter: w, hook: s.afterPublication}, nil
}

type publicationHookWriter struct {
	AtomicWriter
	hook func()
}

func (w *publicationHookWriter) Commit() error {
	if err := w.AtomicWriter.Commit(); err != nil {
		return err
	}
	w.hook()
	return nil
}

func TestPublishedReloadFailureIsNotRetryable(t *testing.T) {
	for _, operation := range []string{"Add", "Addition.Commit", "CompactAll"} {
		for _, cause := range []error{ErrReloadTimeout, fmt.Errorf("storage: %w", ErrLockFailure)} {
			t.Run(fmt.Sprintf("%s/%v", operation, cause), func(t *testing.T) {
				dir := t.TempDir()
				storage := &publicationFailureStorage{Storage: NewLocalStorage(dir)}
				st := regressionStack(t, storage)
				regressionAddRef(t, st, "refs/heads/a")
				if operation == "CompactAll" {
					regressionAddRef(t, st, "refs/heads/b")
				}
				storage.afterPublication = func() { storage.readErr = cause }
				calls := 0
				write := func(w *Writer) error {
					calls++
					index := st.NextUpdateIndex()
					w.SetLimits(index, index)
					return w.AddRef(&RefRecord{RefName: "refs/heads/b", UpdateIndex: index, Value: testHash(2)})
				}
				var err error
				switch operation {
				case "Add":
					// Typical client retry policy must not replay the callback.
					for attempts := 0; attempts < 2; attempts++ {
						err = st.Add(write)
						if !errors.Is(err, ErrLockFailure) {
							break
						}
					}
				case "Addition.Commit":
					tr, openErr := st.NewAddition()
					if openErr != nil {
						t.Fatal(openErr)
					}
					defer tr.Close()
					if err := tr.Add(write); err != nil {
						t.Fatal(err)
					}
					err = tr.Commit()
				case "CompactAll":
					err = st.CompactAll(nil)
				}
				requirePostCommitError(t, err, cause)
				if operation != "CompactAll" && calls != 1 {
					t.Fatalf("write callback ran %d times, want once", calls)
				}
				storage.readErr = nil
				storage.afterPublication = nil
				fresh := regressionStack(t, NewLocalStorage(dir))
				regressionRequireRef(t, fresh.Merged(), "refs/heads/a")
				regressionRequireRef(t, fresh.Merged(), "refs/heads/b")
				if fresh.NextUpdateIndex() != 3 {
					t.Fatalf("unexpected update-index advancement: %d", fresh.NextUpdateIndex())
				}
				if operation == "CompactAll" && len(fresh.stack) != 1 {
					t.Fatal("compacted manifest was not published")
				}
			})
		}
	}
}

func TestAutoCompactionFailureAfterAdditionIsNotRetryable(t *testing.T) {
	storage := &publicationFailureStorage{Storage: NewLocalStorage(t.TempDir())}
	st := regressionStack(t, storage)
	regressionAddRef(t, st, "refs/heads/a")
	st.disableAutoCompact = false
	storage.manifestLocks = 0
	storage.failLockAt = 2 // First lock publishes the addition; second is maintenance.
	err := st.Add(func(w *Writer) error {
		w.SetLimits(2, 2)
		return w.AddRef(&RefRecord{RefName: "refs/heads/b", UpdateIndex: 2, Value: testHash(2)})
	})
	requirePostCommitError(t, err, ErrLockFailure)
	regressionRequireRef(t, st.Merged(), "refs/heads/b")
}

func TestLockFailureBeforePublicationRemainsRetryable(t *testing.T) {
	storage := NewLocalStorage(t.TempDir())
	st := regressionStack(t, storage)
	lock, err := storage.LockForWrite(listFileName)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	called := false
	err = st.Add(func(w *Writer) error { called = true; return nil })
	if called || !errors.Is(err, ErrLockFailure) || errors.Is(err, ErrPostCommit) {
		t.Fatalf("pre-publication contention: callback=%v, error=%v", called, err)
	}
}

func TestEmptyAdditionDoesNotReportPublication(t *testing.T) {
	storage := &publicationFailureStorage{Storage: NewLocalStorage(t.TempDir())}
	st := regressionStack(t, storage)
	regressionAddRef(t, st, "refs/heads/a")
	regressionAddRef(t, st, "refs/heads/b")
	st.disableAutoCompact = false
	storage.manifestLocks = 0
	storage.failLockAt = 2
	err := st.Add(func(w *Writer) error { return nil })
	if !errors.Is(err, ErrLockFailure) || errors.Is(err, ErrPostCommit) {
		t.Fatalf("nothing was published: got %v, want retryable maintenance error", err)
	}
}

// postCommitError classifies whether a write survived publication, so it must
// recognise a PostCommitError that arrives wrapped. It reaches this function
// through errors.Join and fmt.Errorf("%w", ...) on the compaction and addition
// paths, and a bare type assertion misses both and wraps a second time.
func TestPostCommitErrorDoesNotDoubleWrap(t *testing.T) {
	if got := postCommitError(nil); got != nil {
		t.Errorf("postCommitError(nil) = %v, want nil", got)
	}

	cause := errors.New("directory sync failed")
	once := postCommitError(cause)

	var first *PostCommitError
	if !errors.As(once, &first) {
		t.Fatalf("postCommitError(%v) is not a *PostCommitError", cause)
	}
	if !errors.Is(once, ErrPostCommit) {
		t.Fatal("a post-commit error must satisfy errors.Is(err, ErrPostCommit)")
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"bare", once},
		{"joined_with_nil", errors.Join(once, nil)},
		{"joined_with_error", errors.Join(once, errors.New("reload failed"))},
		{"fmt_wrapped", fmt.Errorf("compact: %w", once)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := postCommitError(tc.err)

			if !errors.Is(got, ErrPostCommit) {
				t.Fatalf("lost the post-commit classification: %v", got)
			}

			var outer *PostCommitError
			if !errors.As(got, &outer) {
				t.Fatalf("no *PostCommitError in %v", got)
			}
			var inner *PostCommitError
			if errors.As(outer.Cause, &inner) {
				t.Errorf("wrapped a second time: Cause is itself a *PostCommitError (%v)", outer.Cause)
			}
			// Note: Unwrap returns ErrPostCommit, not Cause, so the
			// underlying error is reachable through the exported field
			// rather than errors.Is.
			if !errors.Is(outer.Cause, cause) {
				t.Errorf("Cause = %v, want the original %v", outer.Cause, cause)
			}
		})
	}
}
