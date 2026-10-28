package state

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"
)

const activityResolution = 15 * time.Second

func (s *Store) activityPath(tree string) string {
	sum := sha256.Sum256([]byte(tree))
	return filepath.Join(s.Dir, "activity", hex.EncodeToString(sum[:8]))
}

func (s *Store) Touch(tree string, now time.Time) {
	if tree == "" {
		return
	}
	path := s.activityPath(tree)
	if info, err := os.Stat(path); err == nil && now.Sub(info.ModTime()) < activityResolution {
		return
	}
	if err := os.WriteFile(path, []byte(tree+"\n"), 0o644); err != nil {
		if os.MkdirAll(filepath.Dir(path), 0o755) != nil || os.WriteFile(path, []byte(tree+"\n"), 0o644) != nil {
			return
		}
	}
	os.Chtimes(path, now, now)
}

func (s *Store) LastActive(tree string) time.Time {
	info, err := os.Stat(s.activityPath(tree))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}
