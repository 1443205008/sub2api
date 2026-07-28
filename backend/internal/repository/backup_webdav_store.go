package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// NewWebDAVBackupStoreFactory returns a BackupWebDAVStoreFactory that creates WebDAV-backed stores.
func NewWebDAVBackupStoreFactory() service.BackupWebDAVStoreFactory {
	return func(cfg *service.BackupWebDAVConfig) service.BackupObjectStore {
		return NewWebDAVBackupStore(cfg)
	}
}

// WebDAVBackupStore implements service.BackupObjectStore using WebDAV protocol.
// All operations use standard HTTP with Basic Auth — no external dependencies required.
type WebDAVBackupStore struct {
	client   *http.Client
	baseURL  string // normalized: always ends with "/"
	username string
	password string
}

// NewWebDAVBackupStore creates a WebDAVBackupStore from the given config.
func NewWebDAVBackupStore(cfg *service.BackupWebDAVConfig) *WebDAVBackupStore {
	baseURL := strings.TrimRight(cfg.URL, "/") + "/"
	return &WebDAVBackupStore{
		client:   &http.Client{Timeout: 60 * time.Second},
		baseURL:  baseURL,
		username: cfg.Username,
		password: cfg.Password,
	}
}

// fullURL builds the absolute URL for the given object key.
func (s *WebDAVBackupStore) fullURL(key string) string {
	// key 由 buildS3Key 生成，如 "backups/2026/07/27/dbname_20260727_150405.sql.gz"
	// 去掉首尾斜杠再拼，避免双斜杠
	return s.baseURL + strings.TrimLeft(key, "/")
}

// doRequest creates and executes an HTTP request with Basic Auth.
func (s *WebDAVBackupStore) doRequest(ctx context.Context, method, url string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth(s.username, s.password)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return s.client.Do(req)
}

// mkcolAll ensures all intermediate directories in dirPath exist by issuing MKCOL
// for each segment. Already-existing directories (405/409) are silently skipped.
func (s *WebDAVBackupStore) mkcolAll(ctx context.Context, dirPath string) error {
	// dirPath is like "backups/2026/07/27"
	parts := strings.Split(strings.Trim(dirPath, "/"), "/")
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = current + part + "/"
		url := s.baseURL + current
		resp, err := s.doRequest(ctx, "MKCOL", url, nil, "")
		if err != nil {
			return fmt.Errorf("MKCOL %s: %w", url, err)
		}
		_ = resp.Body.Close()
		// 201 Created: ok
		// 405 Method Not Allowed: directory already exists — ok
		// 409 Conflict: parent doesn't exist (shouldn't happen since we go depth-first) — ok to ignore
		if resp.StatusCode != http.StatusCreated &&
			resp.StatusCode != http.StatusMethodNotAllowed &&
			resp.StatusCode != http.StatusConflict {
			return fmt.Errorf("MKCOL %s: unexpected status %d", url, resp.StatusCode)
		}
	}
	return nil
}

// Upload creates intermediate directories then PUTs the file content.
func (s *WebDAVBackupStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	// Ensure parent directory exists
	dirPath := path.Dir(key)
	if dirPath != "." && dirPath != "/" {
		if err := s.mkcolAll(ctx, dirPath); err != nil {
			return 0, fmt.Errorf("create directories: %w", err)
		}
	}

	// Use a counting reader to track uploaded bytes
	cr := &countingReader{r: body}
	resp, err := s.doRequest(ctx, "PUT", s.fullURL(key), cr, contentType)
	if err != nil {
		return 0, fmt.Errorf("WebDAV PUT: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("WebDAV PUT %s: status %d", key, resp.StatusCode)
	}
	return cr.n, nil
}

// Download fetches the file and returns a streaming reader.
func (s *WebDAVBackupStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.doRequest(ctx, "GET", s.fullURL(key), nil, "")
	if err != nil {
		return nil, fmt.Errorf("WebDAV GET: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("WebDAV GET %s: status %d", key, resp.StatusCode)
	}
	return resp.Body, nil
}

// Delete removes the file from WebDAV storage.
func (s *WebDAVBackupStore) Delete(ctx context.Context, key string) error {
	resp, err := s.doRequest(ctx, "DELETE", s.fullURL(key), nil, "")
	if err != nil {
		return fmt.Errorf("WebDAV DELETE: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 204 No Content: success; 404 Not Found: already gone — both acceptable
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("WebDAV DELETE %s: status %d", key, resp.StatusCode)
	}
	return nil
}

// PresignURL is not supported by WebDAV. It always returns an empty string,
// which signals BackupService.GetBackupDownloadURL to use the backend proxy download path instead.
func (s *WebDAVBackupStore) PresignURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}

// HeadBucket checks connectivity by issuing a PROPFIND on the base URL.
func (s *WebDAVBackupStore) HeadBucket(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", s.baseURL, nil)
	if err != nil {
		return fmt.Errorf("create PROPFIND request: %w", err)
	}
	req.SetBasicAuth(s.username, s.password)
	req.Header.Set("Depth", "0")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("WebDAV PROPFIND: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 207 Multi-Status is the normal WebDAV response; 200 OK is also acceptable
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("WebDAV authentication failed: check username and password")
		}
		return fmt.Errorf("WebDAV PROPFIND %s: status %d", s.baseURL, resp.StatusCode)
	}
	return nil
}

// countingReader wraps an io.Reader and counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
