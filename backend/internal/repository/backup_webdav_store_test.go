package repository

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type webDAVRoundTripFunc func(*http.Request) (*http.Response, error)

func (f webDAVRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type webDAVUnknownLengthReader struct {
	io.Reader
}

func webDAVTestResponse(req *http.Request, statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode:    statusCode,
		Status:        http.StatusText(statusCode),
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func TestWebDAVBackupStoreUploadUsesContentLengthAndCleansTempFile(t *testing.T) {
	const (
		username = "backup-user"
		password = "backup-password"
		key      = "/115/文件/backups/backup.sql.gz"
		payload  = "compressed-backup-data"
	)

	store := NewWebDAVBackupStore(&service.BackupWebDAVConfig{
		URL:      "https://dav.example.test/dav/",
		Username: username,
		Password: password,
	})
	require.Zero(t, store.client.Timeout)

	var stagedPath string
	var putCalls int
	store.client.Transport = webDAVRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case "MKCOL":
			return webDAVTestResponse(req, http.StatusMethodNotAllowed, ""), nil
		case http.MethodPut:
			putCalls++
			require.Equal(t, int64(len(payload)), req.ContentLength)
			require.Empty(t, req.TransferEncoding)
			require.Empty(t, req.Header.Get("Transfer-Encoding"))
			require.Equal(t, "application/gzip", req.Header.Get("Content-Type"))
			require.Equal(t, "/dav/115/文件/backups/backup.sql.gz", req.URL.Path)
			gotUsername, gotPassword, ok := req.BasicAuth()
			require.True(t, ok)
			require.Equal(t, username, gotUsername)
			require.Equal(t, password, gotPassword)

			file, ok := req.Body.(*os.File)
			require.True(t, ok)
			stagedPath = file.Name()

			data, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, payload, string(data))
			return webDAVTestResponse(req, http.StatusCreated, ""), nil
		default:
			t.Fatalf("unexpected WebDAV method: %s", req.Method)
			return nil, nil
		}
	})

	body := webDAVUnknownLengthReader{Reader: strings.NewReader(payload)}
	sizeBytes, err := store.Upload(context.Background(), key, body, "application/gzip")
	require.NoError(t, err)
	require.Equal(t, int64(len(payload)), sizeBytes)
	require.Equal(t, 1, putCalls)
	require.NotEmpty(t, stagedPath)
	_, err = os.Stat(stagedPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestWebDAVBackupStoreUploadIncludesServerErrorBody(t *testing.T) {
	store := NewWebDAVBackupStore(&service.BackupWebDAVConfig{
		URL: "https://dav.example.test/dav/",
	})
	store.client.Transport = webDAVRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == "MKCOL" {
			return webDAVTestResponse(req, http.StatusMethodNotAllowed, ""), nil
		}
		return webDAVTestResponse(req, http.StatusInternalServerError, `{"message":"storage driver rejected upload"}`), nil
	})

	_, err := store.Upload(
		context.Background(),
		"/115/文件/backups/backup.sql.gz",
		strings.NewReader("payload"),
		"application/gzip",
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "status 500")
	require.Contains(t, err.Error(), "storage driver rejected upload")
}

func TestWebDAVBackupStoreUploadRejectsMKCOLConflict(t *testing.T) {
	store := NewWebDAVBackupStore(&service.BackupWebDAVConfig{
		URL: "https://dav.example.test/dav/",
	})
	var putCalled bool
	store.client.Transport = webDAVRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPut {
			putCalled = true
		}
		return webDAVTestResponse(req, http.StatusConflict, `{"message":"parent collection missing"}`), nil
	})

	_, err := store.Upload(
		context.Background(),
		"backups/2026/07/31/backup.sql.gz",
		strings.NewReader("payload"),
		"application/gzip",
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "WebDAV MKCOL")
	require.Contains(t, err.Error(), "status 409")
	require.Contains(t, err.Error(), "parent collection missing")
	require.False(t, putCalled)
}
