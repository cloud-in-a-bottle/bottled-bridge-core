// Package files stores media content-addressed by SHA-256, so a file shared in several places is kept once.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Store struct {
	dir string
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func ValidSHA(sha string) bool {
	return shaPattern.MatchString(sha)
}

func (s *Store) Path(sha string) string {
	if !ValidSHA(sha) {
		panic(fmt.Errorf("invalid file hash %q", sha))
	}
	return filepath.Join(s.dir, sha[:2], sha)
}

func (s *Store) Put(data []byte) (string, error) {
	return s.PutStream(func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// PutStream stores whatever write produces, without holding it in memory.
func (s *Store) PutStream(write func(w io.Writer) error) (string, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "upload-")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	hash := sha256.New()
	if err = write(io.MultiWriter(tmp, hash)); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	dest := s.Path(sha)
	if _, err = os.Stat(dest); err == nil {
		return sha, nil
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	return sha, os.Rename(tmp.Name(), dest)
}

func (s *Store) Read(sha string) ([]byte, error) {
	return os.ReadFile(s.Path(sha))
}

func (s *Store) Delete(shas []string) error {
	for _, sha := range shas {
		if err := os.Remove(s.Path(sha)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Size returns the on-disk size of a stored file, or -1 if it isn't there.
func (s *Store) Size(sha string) int64 {
	info, err := os.Stat(s.Path(sha))
	if err != nil {
		return -1
	}
	return info.Size()
}
