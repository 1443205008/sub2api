package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
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
		client:   &http.Client{},
		baseURL:  baseURL,
		username: cfg.Username,
		password: cfg.Password,
	}
}

// fullURL builds the absolute URL for the given object key.
func (s *WebDAVBackupStore) fullURL(key string) string {
	// key 由 buildObjectKey 生成，如 "backups/2026/07/27/dbname_20260727_150405.zip"
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
// for each segment. Already-existing directories (405) are silently skipped.
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
		// 201 Created: ok
		// 405 Method Not Allowed: directory already exists — ok
		if resp.StatusCode != http.StatusCreated &&
			resp.StatusCode != http.StatusMethodNotAllowed {
			err := webDAVResponseError("WebDAV MKCOL "+url, resp)
			_ = resp.Body.Close()
			return err
		}
		_ = resp.Body.Close()
	}
	return nil
}

// Upload stages the incoming stream in a temporary file before PUT. A number of
// WebDAV backends reject chunked uploads, so PUT must carry an exact Content-Length.
func (s *WebDAVBackupStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	// Ensure parent directory exists
	dirPath := path.Dir(key)
	if dirPath != "." && dirPath != "/" {
		if err := s.mkcolAll(ctx, dirPath); err != nil {
			return 0, fmt.Errorf("create directories: %w", err)
		}
	}

	tmp, err := os.CreateTemp("", "sub2api-webdav-backup-*")
	if err != nil {
		return 0, fmt.Errorf("create WebDAV upload temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	sizeBytes, err := io.Copy(tmp, body)
	if err != nil {
		return 0, fmt.Errorf("stage WebDAV upload body: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewind WebDAV upload temp file: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.fullURL(key), tmp)
	if err != nil {
		return 0, fmt.Errorf("create WebDAV PUT request: %w", err)
	}
	req.SetBasicAuth(s.username, s.password)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.ContentLength = sizeBytes

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("WebDAV PUT: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return 0, webDAVResponseError("WebDAV PUT "+key, resp)
	}
	return sizeBytes, nil
}

// UploadFile uploads a local file through the same staged, fixed-length PUT
// path as Upload. WebDAV servers commonly reject chunked PUT requests, so the
// shared implementation deliberately sets Content-Length before sending.
func (s *WebDAVBackupStore) UploadFile(ctx context.Context, key string, filePath string, contentType string) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("open WebDAV upload file: %w", err)
	}
	defer func() { _ = file.Close() }()
	return s.Upload(ctx, key, file, contentType)
}

// Download fetches the file and returns a streaming reader.
func (s *WebDAVBackupStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.doRequest(ctx, "GET", s.fullURL(key), nil, "")
	if err != nil {
		return nil, fmt.Errorf("WebDAV GET: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		err := webDAVResponseError("WebDAV GET "+key, resp)
		_ = resp.Body.Close()
		return nil, err
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
		return webDAVResponseError("WebDAV DELETE "+key, resp)
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
		return webDAVResponseError("WebDAV PROPFIND "+s.baseURL, resp)
	}
	return nil
}

const maxWebDAVErrorBodyBytes = 4 << 10

func webDAVResponseError(operation string, resp *http.Response) error {
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxWebDAVErrorBodyBytes+1))
	if readErr != nil {
		return fmt.Errorf("%s: status %d (read error response: %v)", operation, resp.StatusCode, readErr)
	}

	truncated := len(data) > maxWebDAVErrorBodyBytes
	if truncated {
		data = data[:maxWebDAVErrorBodyBytes]
	}
	detail := strings.TrimSpace(string(data))
	detail = strings.ReplaceAll(detail, "\r", " ")
	detail = strings.ReplaceAll(detail, "\n", " ")
	if detail == "" {
		return fmt.Errorf("%s: status %d", operation, resp.StatusCode)
	}
	if truncated {
		detail += "... (truncated)"
	}
	return fmt.Errorf("%s: status %d: %s", operation, resp.StatusCode, detail)
}
