package repository

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type S3BackupStore struct {
	client *s3.Client
	bucket string
}

type WebDAVBackupStore struct {
	client   *http.Client
	baseURL  string
	username string
	password string
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

// NewBackupStoreFactory returns a BackupObjectStoreFactory that creates storage backends by config type.
func NewBackupStoreFactory() service.BackupObjectStoreFactory {
	return func(ctx context.Context, cfg *service.BackupStorageConfig) (service.BackupObjectStore, error) {
		if cfg == nil {
			return nil, fmt.Errorf("storage config is nil")
		}

		switch cfg.StorageType() {
		case service.BackupStorageTypeWebDAV:
			baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
			if baseURL == "" {
				return nil, fmt.Errorf("webdav base_url is required")
			}
			return &WebDAVBackupStore{
				client:   &http.Client{},
				baseURL:  baseURL,
				username: cfg.Username,
				password: cfg.Password,
			}, nil
		default:
			region := cfg.Region
			if region == "" {
				region = "auto"
			}

			awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
				awsconfig.WithRegion(region),
				awsconfig.WithCredentialsProvider(
					credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
				),
			)
			if err != nil {
				return nil, fmt.Errorf("load aws config: %w", err)
			}

			client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
				if cfg.Endpoint != "" {
					o.BaseEndpoint = &cfg.Endpoint
				}
				if cfg.ForcePathStyle {
					o.UsePathStyle = true
				}
				o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
				o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			})

			return &S3BackupStore{client: client, bucket: cfg.Bucket}, nil
		}
	}
}

// NewS3BackupStoreFactory 保留旧名称，兼容现有 Wire 依赖。
func NewS3BackupStoreFactory() service.BackupObjectStoreFactory {
	return NewBackupStoreFactory()
}

func (s *S3BackupStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	if err != nil {
		return 0, fmt.Errorf("S3 PutObject: %w", err)
	}
	return int64(len(data)), nil
}

func (s *S3BackupStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 GetObject: %w", err)
	}
	return result.Body, nil
}

func (s *S3BackupStore) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	return err
}

func (s *S3BackupStore) PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s.client)
	result, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("presign url: %w", err)
	}
	return result.URL, nil
}

func (s *S3BackupStore) HeadBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: &s.bucket,
	})
	if err != nil {
		return fmt.Errorf("S3 HeadBucket failed: %w", err)
	}
	return nil
}

func (w *WebDAVBackupStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	if err := w.ensureParentDirs(ctx, key); err != nil {
		return 0, err
	}

	counter := &countingReader{reader: body}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, w.objectURL(key), counter)
	if err != nil {
		return 0, fmt.Errorf("build WebDAV PUT request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w.applyAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("WebDAV PUT failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("WebDAV PUT failed: %s", compactHTTPError(resp))
	}
	return counter.count, nil
}

func (w *WebDAVBackupStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.objectURL(key), nil)
	if err != nil {
		return nil, fmt.Errorf("build WebDAV GET request: %w", err)
	}
	w.applyAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("WebDAV GET failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, fmt.Errorf("WebDAV GET failed: %s", compactHTTPError(resp))
	}
	return resp.Body, nil
}

func (w *WebDAVBackupStore) Delete(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, w.objectURL(key), nil)
	if err != nil {
		return fmt.Errorf("build WebDAV DELETE request: %w", err)
	}
	w.applyAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("WebDAV DELETE failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("WebDAV DELETE failed: %s", compactHTTPError(resp))
	}
	return nil
}

func (w *WebDAVBackupStore) PresignURL(context.Context, string, time.Duration) (string, error) {
	return "", fmt.Errorf("WebDAV does not support presigned URLs")
}

func (w *WebDAVBackupStore) HeadBucket(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, w.baseURL, nil)
	if err != nil {
		return fmt.Errorf("build WebDAV HEAD request: %w", err)
	}
	w.applyAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("WebDAV HEAD failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusMultiStatus, http.StatusMovedPermanently, http.StatusFound, http.StatusMethodNotAllowed:
		return nil
	default:
		return fmt.Errorf("WebDAV HEAD failed: %s", compactHTTPError(resp))
	}
}

func (w *WebDAVBackupStore) ensureParentDirs(ctx context.Context, key string) error {
	parts := strings.Split(strings.Trim(strings.TrimSpace(key), "/"), "/")
	if len(parts) <= 1 {
		return nil
	}

	current := ""
	for _, segment := range parts[:len(parts)-1] {
		if segment == "" {
			continue
		}
		if current == "" {
			current = segment
		} else {
			current = current + "/" + segment
		}

		req, err := http.NewRequestWithContext(ctx, "MKCOL", w.objectURL(current), nil)
		if err != nil {
			return fmt.Errorf("build WebDAV MKCOL request: %w", err)
		}
		w.applyAuth(req)

		resp, err := w.client.Do(req)
		if err != nil {
			return fmt.Errorf("WebDAV MKCOL failed: %w", err)
		}
		_ = resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusCreated, http.StatusOK, http.StatusNoContent, http.StatusMethodNotAllowed, http.StatusMovedPermanently, http.StatusFound:
			continue
		default:
			return fmt.Errorf("WebDAV MKCOL failed: %s", resp.Status)
		}
	}
	return nil
}

func (w *WebDAVBackupStore) objectURL(key string) string {
	base, err := url.Parse(w.baseURL)
	if err != nil {
		return w.baseURL
	}

	segments := make([]string, 0, 4)
	for _, part := range strings.Split(strings.Trim(strings.TrimSpace(key), "/"), "/") {
		if part == "" {
			continue
		}
		segments = append(segments, url.PathEscape(part))
	}
	if len(segments) == 0 {
		return base.String()
	}

	escapedPath := strings.Join(segments, "/")
	base.Path = strings.TrimRight(base.Path, "/")
	base.Path = path.Join(base.Path, escapedPath)
	if strings.HasSuffix(key, "/") && !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	return base.String()
}

func (w *WebDAVBackupStore) applyAuth(req *http.Request) {
	if strings.TrimSpace(w.username) != "" || strings.TrimSpace(w.password) != "" {
		req.SetBasicAuth(w.username, w.password)
	}
}

func compactHTTPError(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return resp.Status
	}
	return resp.Status + ": " + msg
}
