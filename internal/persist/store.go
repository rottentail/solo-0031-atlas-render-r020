package persist

import (
	"atlas-render/internal/model"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type Store struct {
	mu    sync.RWMutex
	path  string
	state model.State
	lock  *os.File
	write func(string, []byte) error
}

func Open(path string) (*Store, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	lock, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, fmt.Errorf("state is already opened by another process: %w", e)
	}
	s := &Store{path: path, state: model.NewState(), lock: lock, write: atomicWrite}
	data, e := os.ReadFile(path)
	if e != nil && !os.IsNotExist(e) {
		s.Close()
		return nil, e
	}
	if e == nil {
		if e = json.Unmarshal(data, &s.state); e != nil {
			s.Close()
			return nil, fmt.Errorf("invalid persisted JSON: %w", e)
		}
		if e = Validate(s.state); e != nil {
			s.Close()
			return nil, e
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	e := s.lock.Close()
	s.lock = nil
	return e
}
func (s *Store) Read() model.State { s.mu.RLock(); defer s.mu.RUnlock(); return model.Clone(s.state) }

// Artifact transfers ownership of exactly one committed result record to the
// caller. It copies that record's PNG bytes and nothing else, so the cost of a
// download or a pixel comparison grows with the results being read rather than
// with all scenes, assets, snapshots and pinned job payloads in the state.
//
// The state lock is held only while the record and its bytes are copied. Slow
// consumers (network writes, PNG decoding, pixel comparison) run after it has
// been released, so Purge and other commits never wait for them. The returned
// bytes are detached: a later purge can replace or clear the stored record
// without changing bytes already handed to a reader.
func (s *Store) Artifact(id string) (model.Artifact, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.state.Artifacts[id]
	if !ok {
		return model.Artifact{}, false
	}
	a.PNG = append([]byte(nil), a.PNG...)
	return a, true
}
func (s *Store) Update(fn func(*model.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := model.Clone(s.state)
	if e := fn(&next); e != nil {
		return e
	}
	if e := Validate(next); e != nil {
		return e
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if e = s.write(s.path, b); e != nil {
		return fmt.Errorf("persist state: %w", e)
	}
	s.state = next
	return nil
}
func atomicWrite(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".atlas-write-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(name, path)
}
