package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Store struct {
	Dir string
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) statePath() string  { return filepath.Join(s.Dir, "state.json") }
func (s *Store) lockPath() string   { return filepath.Join(s.Dir, "lock") }
func (s *Store) eventsPath() string { return filepath.Join(s.Dir, "events.log") }

func (s *Store) Read() (*State, error) {
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	st := New()
	if len(data) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.statePath(), err)
	}
	st.normalize()
	return st, nil
}

func (s *Store) Update(fn func(*State) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := s.Read()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.write(st)
}

func (s *Store) lock() (func(), error) {
	file, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock state: %w", err)
	}
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}

func (s *Store) write(st *State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "state-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.statePath())
}

type Event struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Resource string    `json:"resource,omitempty"`
	Tree     string    `json:"tree,omitempty"`
	Label    string    `json:"label,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

func (s *Store) Log(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	file, err := os.OpenFile(s.eventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	file.Write(append(data, '\n'))
}

func (s *Store) Events(since time.Time) ([]Event, error) {
	file, err := os.Open(s.eventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	events := []Event{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var ev Event
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		if !ev.Time.Before(since) {
			events = append(events, ev)
		}
	}
	return events, scanner.Err()
}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
