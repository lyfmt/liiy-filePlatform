package main

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClassifyContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		header   string
		sniff    []byte
		wantKind string
	}{
		{
			name:     "text file",
			filename: "note.txt",
			header:   "text/plain",
			sniff:    []byte("hello world"),
			wantKind: entryKindText,
		},
		{
			name:     "png image",
			filename: "img.png",
			header:   "image/png",
			sniff:    []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'},
			wantKind: entryKindImage,
		},
		{
			name:     "binary file",
			filename: "archive.bin",
			header:   "application/octet-stream",
			sniff:    []byte{0x00, 0x01, 0x02, 0x03, 0x04},
			wantKind: entryKindFile,
		},
		{
			name:     "empty png falls back to extension",
			filename: "img.png",
			header:   "application/octet-stream",
			sniff:    nil,
			wantKind: entryKindImage,
		},
		{
			name:     "empty pdf falls back to header",
			filename: "document",
			header:   "application/pdf",
			sniff:    nil,
			wantKind: entryKindFile,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotKind, _ := classifyContent(tt.filename, tt.header, tt.sniff)
			if gotKind != tt.wantKind {
				t.Fatalf("classifyContent() kind = %q, want %q", gotKind, tt.wantKind)
			}
		})
	}
}

func TestAddFileRemovesPartialFileOnWriteFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	header := &multipart.FileHeader{
		Filename: "broken.bin",
		Header:   make(textproto.MIMEHeader),
	}
	header.Header.Set("Content-Type", "application/octet-stream")

	src := &failingMultipartFile{
		reader:    strings.NewReader("partial-data"),
		failAfter: 4,
		failErr:   errors.New("disk full"),
	}

	if _, err := store.AddFile("", header, src, 2*time.Hour); err == nil {
		t.Fatalf("AddFile() error = nil, want failure")
	}

	files, err := os.ReadDir(filepath.Join(dir, "files"))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("files dir should be empty after failed upload, found %d files", len(files))
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("store should not retain failed entry, got %d entries", len(got))
	}
}

type failingMultipartFile struct {
	reader    *strings.Reader
	failAfter int64
	failErr   error
	readSoFar int64
}

func (f *failingMultipartFile) Read(p []byte) (int, error) {
	if f.readSoFar >= f.failAfter {
		return 0, f.failErr
	}

	remaining := f.failAfter - f.readSoFar
	if remaining < int64(len(p)) {
		p = p[:remaining]
	}

	n, err := f.reader.Read(p)
	f.readSoFar += int64(n)
	if err == io.EOF && f.readSoFar >= f.failAfter {
		return n, f.failErr
	}
	return n, err
}

func (f *failingMultipartFile) ReadAt(p []byte, off int64) (int, error) {
	return f.reader.ReadAt(p, off)
}

func (f *failingMultipartFile) Seek(offset int64, whence int) (int64, error) {
	return f.reader.Seek(offset, whence)
}

func (f *failingMultipartFile) Close() error {
	return nil
}

func TestCleanupExpiredRemovesStaleFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	now := time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	expired := Entry{
		ID:           "expired",
		Key:          "expired",
		OriginalName: "expired.txt",
		StoredName:   "expired.txt",
		Kind:         entryKindText,
		ContentType:  "text/plain; charset=utf-8",
		Size:         3,
		CreatedAt:    now.Add(-26 * time.Hour),
		ExpiresAt:    now.Add(-2 * time.Hour),
	}
	live := Entry{
		ID:           "live",
		Key:          "live",
		OriginalName: "live.txt",
		StoredName:   "live.txt",
		Kind:         entryKindText,
		ContentType:  "text/plain; charset=utf-8",
		Size:         4,
		CreatedAt:    now.Add(-time.Hour),
		ExpiresAt:    now.Add(23 * time.Hour),
	}

	if err := os.WriteFile(filepath.Join(dir, "files", expired.ID), []byte("old"), 0o644); err != nil {
		t.Fatalf("write expired file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", live.ID), []byte("live"), 0o644); err != nil {
		t.Fatalf("write live file: %v", err)
	}

	store.mu.Lock()
	store.entries = []Entry{live, expired}
	if err := store.saveIndexLocked(); err != nil {
		store.mu.Unlock()
		t.Fatalf("saveIndexLocked() error = %v", err)
	}
	store.mu.Unlock()

	if err := store.CleanupExpired(); err != nil {
		t.Fatalf("CleanupExpired() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "files", expired.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired file still exists")
	}

	if _, err := os.Stat(filepath.Join(dir, "files", live.ID)); err != nil {
		t.Fatalf("live file missing: %v", err)
	}

	entries := store.List()
	if len(entries) != 1 || entries[0].ID != live.ID {
		t.Fatalf("entries after cleanup = %+v, want only live entry", entries)
	}
}

func TestHandleUploadAndIndexForText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	srv := &server{
		store:          store,
		maxUploadBytes: 4 * 1024 * 1024,
		maxUploadMB:    4,
	}

	body := &strings.Builder{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("key", "manual-note"); err != nil {
		t.Fatalf("WriteField(key) error = %v", err)
	}
	if err := writer.WriteField("text", "hello public board"); err != nil {
		t.Fatalf("WriteField(text) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	uploadReq := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body.String()))
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	uploadResp := httptest.NewRecorder()
	srv.handleUpload(uploadResp, uploadReq)

	if uploadResp.Code != http.StatusSeeOther {
		t.Fatalf("handleUpload() status = %d, want %d", uploadResp.Code, http.StatusSeeOther)
	}

	indexReq := httptest.NewRequest(http.MethodGet, "/?tab=download&msg=ok", nil)
	indexResp := httptest.NewRecorder()
	srv.handleIndex(indexResp, indexReq)

	if indexResp.Code != http.StatusOK {
		t.Fatalf("handleIndex() status = %d, want %d", indexResp.Code, http.StatusOK)
	}

	html := indexResp.Body.String()
	if !strings.Contains(html, "manual-note") {
		t.Fatalf("index page does not contain key")
	}
	if !strings.Contains(html, "hello public board") {
		t.Fatalf("index page does not contain text content")
	}
	if !strings.Contains(html, "保留: 24 小时") {
		t.Fatalf("index page should show default retention hours")
	}
}

func TestHandleIndexDefaultsToUploadTab(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	srv := &server{
		store:          store,
		maxUploadBytes: 4 * 1024 * 1024,
		maxUploadMB:    4,
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()
	srv.handleIndex(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("handleIndex() status = %d, want %d", resp.Code, http.StatusOK)
	}

	html := resp.Body.String()
	if !strings.Contains(html, ">上传入口<") {
		t.Fatalf("upload tab content missing")
	}
	if strings.Contains(html, ">已上传内容<") {
		t.Fatalf("download tab content should not render on default upload page")
	}
	if !strings.Contains(html, `<script src="/app.js" defer></script>`) {
		t.Fatalf("upload page should include external app.js script")
	}
}

func TestHandleAppJS(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	resp := httptest.NewRecorder()

	handleAppJS(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("handleAppJS() status = %d, want %d", resp.Code, http.StatusOK)
	}
	if got := resp.Header().Get("Content-Type"); !strings.Contains(got, "application/javascript") {
		t.Fatalf("handleAppJS() content-type = %q", got)
	}
	if !strings.Contains(resp.Body.String(), `document.querySelectorAll("[data-open-upload]")`) {
		t.Fatalf("handleAppJS() body missing upload trigger binding")
	}
}

func TestHandleUploadRetentionHours(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	now := time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	srv := &server{
		store:          store,
		maxUploadBytes: 4 * 1024 * 1024,
		maxUploadMB:    4,
	}

	body := &strings.Builder{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("mode", "text")
	_ = writer.WriteField("key", "short-lived")
	_ = writer.WriteField("retain_hours", "3")
	_ = writer.WriteField("text", "temporary text")
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	srv.handleUpload(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("handleUpload() status = %d, want %d", resp.Code, http.StatusSeeOther)
	}

	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("entries count = %d, want 1", len(entries))
	}
	if got := int(entries[0].ExpiresAt.Sub(entries[0].CreatedAt) / time.Hour); got != 3 {
		t.Fatalf("retention hours = %d, want 3", got)
	}
}

func TestHandleIndexReflectsUpdatedMaxUploadLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := NewStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	if err := writeConfigFile(configPath, configFile{
		Port:        defaultPort,
		DataDir:     dir,
		MaxUploadMB: 5,
		AuthEnabled: false,
		Username:    "user0001",
		Password:    "pass0001",
	}); err != nil {
		t.Fatalf("writeConfigFile() error = %v", err)
	}

	srv := &server{
		store:          store,
		maxUploadBytes: 32 * 1024 * 1024,
		maxUploadMB:    32,
		configPath:     configPath,
	}

	req := httptest.NewRequest(http.MethodGet, "/?tab=upload&retain="+strconv.Itoa(defaultRetentionHours()), nil)
	resp := httptest.NewRecorder()
	srv.handleIndex(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("handleIndex() status = %d, want %d", resp.Code, http.StatusOK)
	}
	if !strings.Contains(resp.Body.String(), "当前单次上传大小限制 5 MB") {
		t.Fatalf("upload page should reflect updated max_upload_mb from config")
	}
}
