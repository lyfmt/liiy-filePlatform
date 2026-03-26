package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	entryKindText  = "text"
	entryKindImage = "image"
	entryKindFile  = "file"
)

type Entry struct {
	ID           string    `json:"id"`
	Key          string    `json:"key"`
	OriginalName string    `json:"original_name"`
	StoredName   string    `json:"stored_name"`
	Kind         string    `json:"kind"`
	ContentType  string    `json:"content_type"`
	Size         int64     `json:"size"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type Store struct {
	mu       sync.RWMutex
	baseDir  string
	filesDir string
	index    string
	ttl      time.Duration
	now      func() time.Time
	entries  []Entry
}

func NewStore(baseDir string, ttl time.Duration) (*Store, error) {
	if ttl <= 0 {
		return nil, errors.New("ttl must be positive")
	}

	store := &Store{
		baseDir:  baseDir,
		filesDir: filepath.Join(baseDir, "files"),
		index:    filepath.Join(baseDir, "index.json"),
		ttl:      ttl,
		now:      time.Now,
	}

	if err := os.MkdirAll(store.filesDir, 0o755); err != nil {
		return nil, fmt.Errorf("create files dir: %w", err)
	}

	if err := store.loadIndex(); err != nil {
		return nil, err
	}

	if err := store.CleanupExpired(); err != nil {
		return nil, err
	}

	return store, nil
}

func (s *Store) loadIndex() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.index)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.entries = nil
			return nil
		}
		return fmt.Errorf("read index: %w", err)
	}

	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse index: %w", err)
	}

	slices.SortFunc(entries, func(a, b Entry) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	s.entries = entries
	return nil
}

func (s *Store) AddText(key, content string, ttl time.Duration) (Entry, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if strings.TrimSpace(content) == "" {
		return Entry{}, errors.New("text content is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := newID()
	if err != nil {
		return Entry{}, err
	}

	now := s.now().UTC()
	if strings.TrimSpace(key) == "" {
		key = defaultTextKey(now)
	}
	ttl = s.normalizeTTL(ttl)

	name := fmt.Sprintf("%s.txt", id)
	entry := Entry{
		ID:           id,
		Key:          sanitizeKey(key),
		OriginalName: name,
		StoredName:   name,
		Kind:         entryKindText,
		ContentType:  "text/plain; charset=utf-8",
		Size:         int64(len(content)),
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
	}

	dstPath := s.entryPath(entry.ID)
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return Entry{}, fmt.Errorf("create text file: %w", err)
	}

	keepFile := false
	defer func() {
		if !keepFile {
			_ = os.Remove(dstPath)
		}
	}()

	if _, err := io.WriteString(dst, content); err != nil {
		_ = dst.Close()
		return Entry{}, fmt.Errorf("write text file: %w", err)
	}
	if err := dst.Close(); err != nil {
		return Entry{}, fmt.Errorf("close text file: %w", err)
	}

	s.entries = append([]Entry{entry}, s.entries...)
	if err := s.saveIndexLocked(); err != nil {
		s.entries = s.entries[1:]
		return Entry{}, err
	}

	keepFile = true
	return entry, nil
}

func (s *Store) AddFile(key string, header *multipart.FileHeader, src multipart.File, ttl time.Duration) (Entry, error) {
	if header == nil {
		return Entry{}, errors.New("missing file header")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := newID()
	if err != nil {
		return Entry{}, err
	}

	now := s.now().UTC()
	originalName := sanitizeFilename(header.Filename)
	if originalName == "" {
		originalName = fmt.Sprintf("upload-%s.bin", id)
	}

	if strings.TrimSpace(key) == "" {
		key = originalName
	}
	ttl = s.normalizeTTL(ttl)

	dstPath := s.entryPath(id)
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return Entry{}, fmt.Errorf("create upload file: %w", err)
	}

	keepFile := false
	defer func() {
		if !keepFile {
			_ = os.Remove(dstPath)
		}
	}()

	sniff := make([]byte, 4096)
	n, readErr := src.Read(sniff)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		_ = dst.Close()
		return Entry{}, fmt.Errorf("read upload data: %w", readErr)
	}

	sniff = sniff[:n]
	if _, err := dst.Write(sniff); err != nil {
		_ = dst.Close()
		return Entry{}, fmt.Errorf("write upload data: %w", err)
	}

	written, err := io.Copy(dst, src)
	if err != nil {
		_ = dst.Close()
		return Entry{}, fmt.Errorf("persist upload data: %w", err)
	}
	if err := dst.Close(); err != nil {
		return Entry{}, fmt.Errorf("close upload file: %w", err)
	}

	kind, contentType := classifyContent(originalName, header.Header.Get("Content-Type"), sniff)
	entry := Entry{
		ID:           id,
		Key:          sanitizeKey(key),
		OriginalName: originalName,
		StoredName:   originalName,
		Kind:         kind,
		ContentType:  contentType,
		Size:         int64(len(sniff)) + written,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
	}

	s.entries = append([]Entry{entry}, s.entries...)
	if err := s.saveIndexLocked(); err != nil {
		s.entries = s.entries[1:]
		return Entry{}, err
	}

	keepFile = true
	return entry, nil
}

func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.entries)
}

func (s *Store) Get(id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.now().UTC()
	for _, entry := range s.entries {
		if entry.ID == id && entry.ExpiresAt.After(now) {
			return entry, true
		}
	}

	return Entry{}, false
}

func (s *Store) ReadText(id string) (string, error) {
	data, err := os.ReadFile(s.entryPath(id))
	if err != nil {
		return "", fmt.Errorf("read text content: %w", err)
	}
	return string(data), nil
}

func (s *Store) Open(id string) (*os.File, error) {
	return os.Open(s.entryPath(id))
}

func (s *Store) CleanupExpired() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	kept := make([]Entry, 0, len(s.entries))
	changed := false
	var firstErr error

	for _, entry := range s.entries {
		if entry.ExpiresAt.After(now) {
			kept = append(kept, entry)
			continue
		}

		if err := os.Remove(s.entryPath(entry.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove expired file %s: %w", entry.ID, err)
			}
			kept = append(kept, entry)
			continue
		}

		changed = true
	}

	if !changed {
		return firstErr
	}

	s.entries = kept
	if err := s.saveIndexLocked(); err != nil && firstErr == nil {
		firstErr = err
	}

	return firstErr
}

func (s *Store) StartCleanupLoop(stop <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			_ = s.CleanupExpired()
		}
	}
}

func (s *Store) normalizeTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 || ttl > s.ttl {
		return s.ttl
	}
	return ttl
}

func (s *Store) saveIndexLocked() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize index: %w", err)
	}

	tmpPath := s.index + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write temp index: %w", err)
	}

	if err := os.Rename(tmpPath, s.index); err != nil {
		return fmt.Errorf("replace index: %w", err)
	}

	return nil
}

func (s *Store) entryPath(id string) string {
	return filepath.Join(s.filesDir, id)
}

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func defaultTextKey(now time.Time) string {
	return "text-" + now.UTC().Format("20060102-150405")
}

func sanitizeKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "\n", " ")
	key = strings.ReplaceAll(key, "\r", " ")
	if key == "" {
		return "untitled"
	}
	return key
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == 0 || r == '/' {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

func classifyContent(filename, headerType string, sniff []byte) (string, string) {
	contentType := ""
	if len(sniff) > 0 {
		contentType = strings.ToLower(strings.TrimSpace(http.DetectContentType(sniff)))
	}

	extType := strings.ToLower(strings.TrimSpace(mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))))
	headerMediaType := ""
	if mediaType, _, err := mime.ParseMediaType(headerType); err == nil {
		headerMediaType = strings.ToLower(strings.TrimSpace(mediaType))
	}

	if len(sniff) == 0 {
		switch {
		case extType != "":
			contentType = extType
		case headerMediaType != "":
			contentType = headerMediaType
		default:
			contentType = "application/octet-stream"
		}
	} else {
		if contentType == "application/octet-stream" || contentType == "" {
			if extType != "" {
				contentType = extType
			}
		}
		if contentType == "application/octet-stream" && headerMediaType != "" {
			contentType = headerMediaType
		}
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}

	switch {
	case contentType == "image/svg+xml":
		return entryKindFile, contentType
	case strings.HasPrefix(contentType, "image/"):
		return entryKindImage, contentType
	case isTextMediaType(contentType) || (len(sniff) > 0 && looksLikeText(sniff)):
		return entryKindText, contentType
	default:
		return entryKindFile, contentType
	}
}

func isTextMediaType(contentType string) bool {
	if strings.HasPrefix(contentType, "text/") {
		return true
	}

	switch contentType {
	case "application/json",
		"application/xml",
		"application/javascript",
		"application/x-yaml",
		"application/yaml",
		"image/svg+xml":
		return true
	default:
		return false
	}
}

func looksLikeText(sample []byte) bool {
	if len(sample) == 0 {
		return true
	}
	if bytes.IndexByte(sample, 0) >= 0 || !utf8.Valid(sample) {
		return false
	}

	printable := 0
	for _, r := range string(sample) {
		if r == '\n' || r == '\r' || r == '\t' {
			printable++
			continue
		}
		if !unicode.IsControl(r) {
			printable++
		}
	}

	return printable*100 >= len([]rune(string(sample)))*90
}
