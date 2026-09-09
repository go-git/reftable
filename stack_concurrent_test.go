package reftable

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A process that serves several repositories holds one Stack per repository
// and writes to them from different goroutines. Each Stack is used by a single
// goroutine — Stack itself is not safe for concurrent use — but everything a
// Stack touches that is shared across instances must be.
//
// Run under -race: this is what catches shared writer state such as the
// package-level RNG behind formatName.
func TestConcurrentStacksInSeparateRepos(t *testing.T) {
	const repos = 8
	const writesPerRepo = 6

	dirs := make([]string, repos)
	root := t.TempDir()
	for i := range dirs {
		dirs[i] = filepath.Join(root, fmt.Sprintf("repo%d", i))
		if err := os.MkdirAll(dirs[i], 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, repos)
	start := make(chan struct{})

	for i := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximise overlap

			st, err := NewStack(NewLocalStorage(dirs[i]), Config{})
			if err != nil {
				errs[i] = fmt.Errorf("NewStack: %w", err)
				return
			}
			defer st.Close()

			for j := range writesPerRepo {
				name := fmt.Sprintf("refs/heads/repo%d-%d", i, j)
				err := st.Add(func(w *Writer) error {
					idx := st.NextUpdateIndex()
					w.SetLimits(idx, idx)
					return w.AddRef(&RefRecord{
						RefName:     name,
						UpdateIndex: idx,
						Value:       testHash(i*writesPerRepo + j),
					})
				})
				if err != nil {
					errs[i] = fmt.Errorf("Add(%s): %w", name, err)
					return
				}
			}

			// Every ref this goroutine wrote must be readable from its stack.
			for j := range writesPerRepo {
				name := fmt.Sprintf("refs/heads/repo%d-%d", i, j)
				rec, err := ReadRef(st.Merged(), name)
				if err != nil {
					errs[i] = fmt.Errorf("ReadRef(%s): %w", name, err)
					return
				}
				if rec == nil {
					errs[i] = fmt.Errorf("ref %s missing after concurrent writes", name)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("repo%d: %v", i, err)
		}
	}
}

// Table names must be unique across concurrently-created additions: they are
// the filenames the manifest refers to, and a collision loses a table.
func TestConcurrentTableNamesAreUnique(t *testing.T) {
	const goroutines = 16
	const each = 64

	var mu sync.Mutex
	seen := map[string]bool{}
	dup := ""

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, each)
			for j := range each {
				local = append(local, formatName(uint64(j), uint64(j)))
			}
			mu.Lock()
			defer mu.Unlock()
			for _, n := range local {
				if seen[n] && dup == "" {
					dup = n
				}
				seen[n] = true
			}
		}()
	}
	wg.Wait()

	if dup != "" {
		t.Errorf("duplicate table name generated concurrently: %s", dup)
	}
}
