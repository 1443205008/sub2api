package service

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	settingKeyBackupS3Config     = "backup_s3_config"
	settingKeyBackupSchedule     = "backup_schedule"
	settingKeyBackupRecords      = "backup_records"
	settingKeyBackupStorageType  = "backup_storage_type"
	settingKeyBackupWebDAVConfig = "backup_webdav_config"

	maxBackupRecords           = 100
	backupObjectCleanupTimeout = 2 * time.Minute

	// backupScheduledLeaderLockKey gates the scheduled full-database backup so
	// that only one instance in a clustered deployment performs the
	// dump-and-upload each cycle. Without it every instance runs the cron
	// independently, producing N concurrent pg_dumps against the same database,
	// N× peak memory while the archive is uploaded, and N identical objects that
	// overwrite the same timestamped key. Every other periodic job in this
	// package is already gated the same way; the scheduled backup was the last
	// one that still fanned out across every instance.
	backupScheduledLeaderLockKey = "backup:scheduled:leader"
	// backupScheduledLeaderLockTTL bounds crash recovery only; the lock is
	// released as soon as the backup finishes. It must exceed the job's
	// worst-case runtime (the scheduled backup context is bounded at 30m) so the
	// lock cannot expire mid-dump and let a peer start a second backup.
	backupScheduledLeaderLockTTL = 35 * time.Minute
)

// BackupStorageType identifies the object storage backend used for backups.
type BackupStorageType string

const (
	BackupStorageTypeS3     BackupStorageType = "s3"
	BackupStorageTypeWebDAV BackupStorageType = "webdav"
)

var (
	ErrBackupS3NotConfigured     = infraerrors.BadRequest("BACKUP_S3_NOT_CONFIGURED", "backup S3 storage is not configured")
	ErrBackupWebDAVNotConfigured = infraerrors.BadRequest("BACKUP_WEBDAV_NOT_CONFIGURED", "backup WebDAV storage is not configured")
	ErrBackupNotFound            = infraerrors.NotFound("BACKUP_NOT_FOUND", "backup record not found")
	ErrBackupInProgress          = infraerrors.Conflict("BACKUP_IN_PROGRESS", "a backup is already in progress")
	ErrRestoreInProgress         = infraerrors.Conflict("RESTORE_IN_PROGRESS", "a restore is already in progress")
	ErrBackupRecordsCorrupt      = infraerrors.InternalServer("BACKUP_RECORDS_CORRUPT", "backup records data is corrupted")
	ErrBackupS3ConfigCorrupt     = infraerrors.InternalServer("BACKUP_S3_CONFIG_CORRUPT", "backup S3 config data is corrupted")
	ErrBackupWebDAVConfigCorrupt = infraerrors.InternalServer("BACKUP_WEBDAV_CONFIG_CORRUPT", "backup WebDAV config data is corrupted")

	// ErrSecretEncryptionKeyNotConfigured is returned when an S3 SecretAccessKey
	// would be encrypted with an auto-generated (ephemeral) key. That key is
	// regenerated on every process start, so the persisted ciphertext becomes
	// undecryptable after a restart/upgrade ("cipher: message authentication
	// failed"), silently breaking S3 backup/image storage (#4524). Mirrors the
	// existing guards for payments (payment.ProvideEncryptionKey) and TOTP
	// enablement, which likewise refuse to depend on an auto-generated key.
	ErrSecretEncryptionKeyNotConfigured = infraerrors.BadRequest(
		"SECRET_ENCRYPTION_KEY_NOT_CONFIGURED",
		"cannot store the S3 secret access key: no fixed secret encryption key is configured, so the auto-generated key would change on every restart and make the stored secret undecryptable after a restart or upgrade. Set a fixed TOTP_ENCRYPTION_KEY (e.g. generate one with `openssl rand -hex 32`) and try again",
	)
)

// ─── 接口定义 ───

// DBDumper abstracts database dump/restore operations
type DBDumper interface {
	Dump(ctx context.Context) (io.ReadCloser, error)
	Restore(ctx context.Context, data io.Reader) error
}

// BackupObjectStore abstracts object storage for backup files
type BackupObjectStore interface {
	Upload(ctx context.Context, key string, body io.Reader, contentType string) (sizeBytes int64, err error)
	UploadFile(ctx context.Context, key string, filePath string, contentType string) (sizeBytes int64, err error)
	Download(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error)
	HeadBucket(ctx context.Context) error
}

// BackupObjectStoreFactory creates an object store from S3 config
type BackupObjectStoreFactory func(ctx context.Context, cfg *BackupS3Config) (BackupObjectStore, error)

// BackupWebDAVStoreFactory creates a WebDAV object store from config
type BackupWebDAVStoreFactory func(cfg *BackupWebDAVConfig) BackupObjectStore

// ─── 数据模型 ───

// BackupWebDAVConfig WebDAV 存储配置
type BackupWebDAVConfig struct {
	URL      string `json:"url"` // WebDAV 服务器地址，如 https://dav.jianguoyun.com/dav/
	Username string `json:"username"`
	Password string `json:"password,omitempty"` // 存储时加密
	Prefix   string `json:"prefix"`             // 路径前缀，如 "backups"
}

// IsConfigured 检查 WebDAV 必要字段是否已配置
func (c *BackupWebDAVConfig) IsConfigured() bool {
	return c.URL != "" && c.Username != "" && c.Password != ""
}

// BackupS3Config S3 兼容存储配置（支持 Cloudflare R2）
type BackupS3Config struct {
	Endpoint        string `json:"endpoint"` // e.g. https://<account_id>.r2.cloudflarestorage.com
	Region          string `json:"region"`   // R2 用 "auto"
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key,omitempty"` //nolint:revive // field name follows AWS convention
	Prefix          string `json:"prefix"`                      // S3 key 前缀，如 "backups/"
	ForcePathStyle  bool   `json:"force_path_style"`
}

// IsConfigured 检查必要字段是否已配置
func (c *BackupS3Config) IsConfigured() bool {
	return c.Bucket != "" && c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// BackupScheduleConfig 定时备份配置
type BackupScheduleConfig struct {
	Enabled     bool   `json:"enabled"`
	CronExpr    string `json:"cron_expr"`    // cron 表达式，如 "0 2 * * *" 每天凌晨2点
	RetainDays  int    `json:"retain_days"`  // 备份文件过期天数，默认14，0=不自动清理
	RetainCount int    `json:"retain_count"` // 最多保留份数，0=不限制
}

// BackupRecord 备份记录
type BackupRecord struct {
	ID            string       `json:"id"`
	Status        string       `json:"status"`      // pending, running, completed, failed
	BackupType    string       `json:"backup_type"` // postgres
	FileName      string       `json:"file_name"`
	S3Key         string       `json:"s3_key"`
	Parts         []BackupPart `json:"parts,omitempty"`
	SizeBytes     int64        `json:"size_bytes"`
	TriggeredBy   string       `json:"triggered_by"` // manual, scheduled
	ErrorMsg      string       `json:"error_message,omitempty"`
	StartedAt     string       `json:"started_at"`
	FinishedAt    string       `json:"finished_at,omitempty"`
	ExpiresAt     string       `json:"expires_at,omitempty"`     // 过期时间
	Progress      string       `json:"progress,omitempty"`       // "dumping", "uploading", ""
	RestoreStatus string       `json:"restore_status,omitempty"` // "", "running", "completed", "failed"
	RestoreError  string       `json:"restore_error,omitempty"`
	RestoredAt    string       `json:"restored_at,omitempty"`
}

// BackupDownloadPart 描述一个可下载的备份分卷。
type BackupDownloadPart struct {
	Index     int    `json:"index"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
}

// BackupDownloadResponse 是单文件和分卷下载响应的兼容表示。
type BackupDownloadResponse struct {
	URL   string               `json:"url,omitempty"`
	Parts []BackupDownloadPart `json:"parts,omitempty"`
}

// BackupService 数据库备份恢复服务
type BackupService struct {
	settingRepo SettingRepository
	dbCfg       *config.DatabaseConfig
	encryptor   SecretEncryptor
	// encryptionKeyConfigured mirrors cfg.Totp.EncryptionKeyConfigured: false
	// means the secret encryption key was auto-generated and does not survive a
	// restart. Durable-secret writers must refuse to persist new secrets in that
	// mode (#4524).
	encryptionKeyConfigured bool
	storeFactory            BackupObjectStoreFactory
	webdavFactory           BackupWebDAVStoreFactory
	dumper                  DBDumper

	opMu      sync.Mutex // 保护 backingUp/restoring 标志
	backingUp bool
	restoring bool

	storeMu     sync.Mutex // 保护 store/s3Cfg/webdavCfg/storageType 缓存
	store       BackupObjectStore
	s3Cfg       *BackupS3Config
	webdavCfg   *BackupWebDAVConfig
	storageType BackupStorageType // 已缓存的存储类型

	recordsMu sync.Mutex // 保护 records 的 load/save 操作

	cronMu      sync.Mutex
	cronSched   *cron.Cron
	cronEntryID cron.EntryID

	// lockCache/db elect a single leader for the scheduled backup across
	// instances; instanceID identifies this process as the lock owner. Injected
	// via SetLeaderLock — when both are nil the backup runs ungated
	// (single-instance / test behavior).
	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string

	wg            sync.WaitGroup     // 追踪活跃的备份/恢复 goroutine
	shuttingDown  atomic.Bool        // 阻止新备份启动
	bgCtx         context.Context    // 所有后台操作的 parent context
	bgCancel      context.CancelFunc // 取消所有活跃后台操作
	partSizeBytes int64              // 分卷阈值；生产使用 4 GiB，测试可注入更小值
}

func NewBackupService(
	settingRepo SettingRepository,
	cfg *config.Config,
	encryptor SecretEncryptor,
	storeFactory BackupObjectStoreFactory,
	webdavFactory BackupWebDAVStoreFactory,
	dumper DBDumper,
) *BackupService {
	bgCtx, bgCancel := context.WithCancel(context.Background())
	return &BackupService{
		settingRepo:             settingRepo,
		dbCfg:                   &cfg.Database,
		encryptor:               encryptor,
		encryptionKeyConfigured: cfg.Totp.EncryptionKeyConfigured,
		storeFactory:            storeFactory,
		webdavFactory:           webdavFactory,
		dumper:                  dumper,
		bgCtx:                   bgCtx,
		bgCancel:                bgCancel,
		partSizeBytes:           defaultBackupPartSizeBytes,
		instanceID:              uuid.NewString(),
	}
}

// SetLeaderLock injects the leader-lock cache and DB used to elect a single
// instance for the scheduled backup. When both are nil the scheduled backup runs
// ungated (single-instance / test behavior).
func (s *BackupService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// Start 启动定时备份调度器并清理孤立记录
func (s *BackupService) Start() {
	s.cronSched = cron.New()
	s.cronSched.Start()

	// 清理重启后孤立的 running 记录
	s.recoverStaleRecords()

	// 加载已有的定时配置
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schedule, err := s.GetSchedule(ctx)
	if err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 加载定时备份配置失败: %v", err)
		return
	}
	if schedule.Enabled && schedule.CronExpr != "" {
		if err := s.applyCronSchedule(schedule); err != nil {
			logger.LegacyPrintf("service.backup", "[Backup] 应用定时备份配置失败: %v", err)
		}
	}
}

// recoverStaleRecords 启动时将孤立的 running 记录标记为 failed，并清理已上传对象。
func (s *BackupService) recoverStaleRecords() {
	loadCtx, loadCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer loadCancel()

	records, err := s.loadRecords(loadCtx)
	if err != nil {
		return
	}
	for i := range records {
		if records[i].Status == "running" {
			staleRecord := records[i]
			records[i].Status = "failed"
			records[i].ErrorMsg = "interrupted by server restart"
			records[i].Progress = ""
			records[i].FinishedAt = time.Now().Format(time.RFC3339)
			s.saveRecoveredRecord(&records[i])

			if cleanupErr := s.cleanupStaleBackupObjects(&staleRecord); cleanupErr != nil {
				records[i].ErrorMsg = fmt.Sprintf("interrupted by server restart; cleanup failed, manual deletion may be required: %v", cleanupErr)
				s.saveRecoveredRecord(&records[i])
				logger.LegacyPrintf("service.backup", "[Backup] failed to clean stale backup objects for %s: %v", records[i].ID, cleanupErr)
			}
			logger.LegacyPrintf("service.backup", "[Backup] recovered stale running record: %s", records[i].ID)
		}
		if records[i].RestoreStatus == "running" {
			records[i].RestoreStatus = "failed"
			records[i].RestoreError = "interrupted by server restart"
			s.saveRecoveredRecord(&records[i])
			logger.LegacyPrintf("service.backup", "[Backup] recovered stale restoring record: %s", records[i].ID)
		}
	}
}

func (s *BackupService) saveRecoveredRecord(record *BackupRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.saveRecord(ctx, record); err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 保存恢复后的备份记录失败 %s: %v", record.ID, err)
	}
}

func (s *BackupService) cleanupStaleBackupObjects(record *BackupRecord) error {
	if len(backupObjectKeys(record)) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), backupObjectCleanupTimeout)
	defer cancel()
	return s.deleteBackupObjects(ctx, record)
}

// Stop 停止定时备份并等待活跃操作完成
func (s *BackupService) Stop() {
	s.shuttingDown.Store(true)

	s.cronMu.Lock()
	if s.cronSched != nil {
		s.cronSched.Stop()
	}
	s.cronMu.Unlock()

	// 等待活跃备份/恢复完成（最多 5 分钟）
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logger.LegacyPrintf("service.backup", "[Backup] all active operations finished")
	case <-time.After(5 * time.Minute):
		logger.LegacyPrintf("service.backup", "[Backup] shutdown timeout after 5min, cancelling active operations")
		if s.bgCancel != nil {
			s.bgCancel() // 取消所有后台操作
		}
		// 给 goroutine 时间响应取消并完成清理
		select {
		case <-done:
			logger.LegacyPrintf("service.backup", "[Backup] active operations cancelled and cleaned up")
		case <-time.After(10 * time.Second):
			logger.LegacyPrintf("service.backup", "[Backup] goroutine cleanup timed out")
		}
	}
}

// ─── S3 配置管理 ───

// EncryptionKeyConfigured reports whether a fixed (explicitly configured) secret
// encryption key is in use. When false the key is auto-generated on every start
// and secrets encrypted with it cannot be recovered after a restart, so callers
// that persist durable secrets must refuse to do so (#4524).
func (s *BackupService) EncryptionKeyConfigured() bool {
	return s != nil && s.encryptionKeyConfigured
}

func (s *BackupService) GetS3Config(ctx context.Context) (*BackupS3Config, error) {
	cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &BackupS3Config{}, nil
	}
	// 脱敏返回
	cfg.SecretAccessKey = ""
	return cfg, nil
}

func (s *BackupService) UpdateS3Config(ctx context.Context, cfg BackupS3Config) (*BackupS3Config, error) {
	// 如果没提供 secret，保留原有值
	if cfg.SecretAccessKey == "" {
		old, _ := s.loadS3Config(ctx)
		if old != nil {
			cfg.SecretAccessKey = old.SecretAccessKey
		}
	} else {
		// 拒绝用自动生成的临时密钥加密：该密钥每次重启都会变化，落库的密文在
		// 重启/升级后无法解密（#4524）。与支付、TOTP 的处理保持一致。
		if !s.encryptionKeyConfigured {
			return nil, ErrSecretEncryptionKeyNotConfigured
		}
		// 加密 SecretAccessKey
		encrypted, err := s.encryptor.Encrypt(cfg.SecretAccessKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt secret: %w", err)
		}
		cfg.SecretAccessKey = encrypted
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal s3 config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, settingKeyBackupS3Config, string(data)); err != nil {
		return nil, fmt.Errorf("save s3 config: %w", err)
	}

	// 清除缓存的 S3 客户端
	s.storeMu.Lock()
	s.store = nil
	s.s3Cfg = nil
	s.webdavCfg = nil
	s.storeMu.Unlock()

	cfg.SecretAccessKey = ""
	return &cfg, nil
}

func (s *BackupService) TestS3Connection(ctx context.Context, cfg BackupS3Config) error {
	// 如果没提供 secret，用已保存的
	if cfg.SecretAccessKey == "" {
		old, _ := s.loadS3Config(ctx)
		if old != nil {
			cfg.SecretAccessKey = old.SecretAccessKey
		}
	}

	if cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return fmt.Errorf("incomplete S3 config: bucket, access_key_id, secret_access_key are required")
	}

	store, err := s.storeFactory(ctx, &cfg)
	if err != nil {
		return err
	}
	return store.HeadBucket(ctx)
}

// ─── WebDAV 配置管理 ───

func (s *BackupService) GetStorageType(ctx context.Context) (BackupStorageType, error) {
	t, err := s.loadStorageType(ctx)
	if err != nil {
		return "", err
	}
	return t, nil
}

func (s *BackupService) UpdateStorageType(ctx context.Context, t BackupStorageType) error {
	if t != BackupStorageTypeS3 && t != BackupStorageTypeWebDAV {
		return infraerrors.BadRequest("INVALID_STORAGE_TYPE", "storage type must be 's3' or 'webdav'")
	}
	if err := s.settingRepo.Set(ctx, settingKeyBackupStorageType, string(t)); err != nil {
		return fmt.Errorf("save storage type: %w", err)
	}
	// 清除已缓存的 store，使下次 getOrCreateStore 重新初始化
	s.storeMu.Lock()
	s.store = nil
	s.s3Cfg = nil
	s.webdavCfg = nil
	s.storeMu.Unlock()
	return nil
}

func (s *BackupService) GetWebDAVConfig(ctx context.Context) (*BackupWebDAVConfig, error) {
	cfg, err := s.loadWebDAVConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &BackupWebDAVConfig{}, nil
	}
	// 脱敏返回
	cfg.Password = ""
	return cfg, nil
}

func (s *BackupService) UpdateWebDAVConfig(ctx context.Context, cfg BackupWebDAVConfig) (*BackupWebDAVConfig, error) {
	if cfg.Password == "" {
		old, _ := s.loadWebDAVConfig(ctx)
		if old != nil {
			cfg.Password = old.Password
		}
	} else {
		if !s.encryptionKeyConfigured {
			return nil, ErrSecretEncryptionKeyNotConfigured
		}
		encrypted, err := s.encryptor.Encrypt(cfg.Password)
		if err != nil {
			return nil, fmt.Errorf("encrypt password: %w", err)
		}
		cfg.Password = encrypted
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal webdav config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, settingKeyBackupWebDAVConfig, string(data)); err != nil {
		return nil, fmt.Errorf("save webdav config: %w", err)
	}

	s.storeMu.Lock()
	s.store = nil
	s.webdavCfg = nil
	s.storeMu.Unlock()

	cfg.Password = ""
	return &cfg, nil
}

func (s *BackupService) TestWebDAVConnection(ctx context.Context, cfg BackupWebDAVConfig) error {
	if cfg.Password == "" {
		old, _ := s.loadWebDAVConfig(ctx)
		if old != nil {
			cfg.Password = old.Password
		}
	}
	if cfg.URL == "" || cfg.Username == "" || cfg.Password == "" {
		return fmt.Errorf("incomplete WebDAV config: url, username, password are required")
	}
	store := s.webdavFactory(&cfg)
	return store.HeadBucket(ctx)
}

// ─── 定时备份管理 ───

func (s *BackupService) GetSchedule(ctx context.Context) (*BackupScheduleConfig, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupSchedule)
	if err != nil || raw == "" {
		return &BackupScheduleConfig{}, nil
	}
	var cfg BackupScheduleConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return &BackupScheduleConfig{}, nil
	}
	return &cfg, nil
}

func (s *BackupService) UpdateSchedule(ctx context.Context, cfg BackupScheduleConfig) (*BackupScheduleConfig, error) {
	if cfg.Enabled && cfg.CronExpr == "" {
		return nil, infraerrors.BadRequest("INVALID_CRON", "cron expression is required when schedule is enabled")
	}
	// 验证 cron 表达式
	if cfg.CronExpr != "" {
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		if _, err := parser.Parse(cfg.CronExpr); err != nil {
			return nil, infraerrors.BadRequest("INVALID_CRON", fmt.Sprintf("invalid cron expression: %v", err))
		}
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal schedule config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, settingKeyBackupSchedule, string(data)); err != nil {
		return nil, fmt.Errorf("save schedule config: %w", err)
	}

	// 应用或停止定时任务
	if cfg.Enabled {
		if err := s.applyCronSchedule(&cfg); err != nil {
			return nil, err
		}
	} else {
		s.removeCronSchedule()
	}

	return &cfg, nil
}

func (s *BackupService) applyCronSchedule(cfg *BackupScheduleConfig) error {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()

	if s.cronSched == nil {
		return fmt.Errorf("cron scheduler not initialized")
	}

	// 移除旧任务
	if s.cronEntryID != 0 {
		s.cronSched.Remove(s.cronEntryID)
		s.cronEntryID = 0
	}

	entryID, err := s.cronSched.AddFunc(cfg.CronExpr, func() {
		s.runScheduledBackup()
	})
	if err != nil {
		return infraerrors.BadRequest("INVALID_CRON", fmt.Sprintf("failed to schedule: %v", err))
	}
	s.cronEntryID = entryID
	logger.LegacyPrintf("service.backup", "[Backup] 定时备份已启用: %s", cfg.CronExpr)
	return nil
}

func (s *BackupService) removeCronSchedule() {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	if s.cronSched != nil && s.cronEntryID != 0 {
		s.cronSched.Remove(s.cronEntryID)
		s.cronEntryID = 0
		logger.LegacyPrintf("service.backup", "[Backup] 定时备份已停用")
	}
}

func (s *BackupService) runScheduledBackup() {
	s.wg.Add(1)
	defer s.wg.Done()

	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Minute)
	defer cancel()

	// 多实例保护: 集群部署时只让 leader 执行定时备份, 避免每个实例各自对同一个
	// 数据库跑一次全量 dump、上传时峰值内存翻倍、以及多份同名对象互相覆盖。
	// 手动触发的备份 (CreateBackup/StartBackup) 不受此限, 运维仍可随时在任一节点强制备份。
	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, backupScheduledLeaderLockKey, s.instanceID, backupScheduledLeaderLockTTL)
	if !ok {
		logger.LegacyPrintf("service.backup", "[Backup] 定时备份跳过: 本实例非 leader")
		return
	}
	defer release()

	// 读取定时备份配置中的过期天数
	schedule, _ := s.GetSchedule(ctx)
	expireDays := 14 // 默认14天过期
	if schedule != nil && schedule.RetainDays > 0 {
		expireDays = schedule.RetainDays
	}

	logger.LegacyPrintf("service.backup", "[Backup] 开始执行定时备份, 过期天数: %d", expireDays)
	record, err := s.CreateBackup(ctx, "scheduled", expireDays)
	if err != nil {
		if errors.Is(err, ErrBackupInProgress) {
			logger.LegacyPrintf("service.backup", "[Backup] 定时备份跳过: 已有备份正在进行中")
		} else {
			logger.LegacyPrintf("service.backup", "[Backup] 定时备份失败: %v", err)
		}
		return
	}
	logger.LegacyPrintf("service.backup", "[Backup] 定时备份完成: id=%s size=%d", record.ID, record.SizeBytes)

	// 清理过期备份（复用已加载的 schedule）
	if schedule == nil {
		return
	}
	if err := s.cleanupOldBackups(ctx, schedule); err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 清理过期备份失败: %v", err)
	}
}

// ─── 备份/恢复核心 ───

// CreateBackup 创建全量数据库备份并上传到 S3。
// expireDays: 备份过期天数，0=永不过期，默认14天
func (s *BackupService) CreateBackup(ctx context.Context, triggeredBy string, expireDays int) (*BackupRecord, error) {
	if s.shuttingDown.Load() {
		return nil, infraerrors.ServiceUnavailable("SERVER_SHUTTING_DOWN", "server is shutting down")
	}

	s.opMu.Lock()
	if s.backingUp {
		s.opMu.Unlock()
		return nil, ErrBackupInProgress
	}
	s.backingUp = true
	s.opMu.Unlock()
	defer func() {
		s.opMu.Lock()
		s.backingUp = false
		s.opMu.Unlock()
	}()

	objectStore, prefix, err := s.getOrCreateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("init object store: %w", err)
	}
	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	backupID := uuid.New().String()[:8]
	fileName := fmt.Sprintf("%s_%s.zip", s.dbCfg.DBName, now.Format("20060102_150405"))
	s3Key := buildObjectKey(prefix, fileName)

	var expiresAt string
	if expireDays > 0 {
		expiresAt = now.AddDate(0, 0, expireDays).Format(time.RFC3339)
	}

	record := &BackupRecord{
		ID:          backupID,
		Status:      "running",
		BackupType:  "postgres",
		FileName:    fileName,
		S3Key:       s3Key,
		TriggeredBy: triggeredBy,
		StartedAt:   now.Format(time.RFC3339),
		ExpiresAt:   expiresAt,
	}

	archivePath, sizeBytes, err := s.createCompressedBackupFile(ctx)
	if err != nil {
		record.Status = "failed"
		record.ErrorMsg = err.Error()
		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(ctx, record)
		return record, err
	}
	defer func() { _ = cleanupBackupFiles(archivePath) }()
	record.SizeBytes = sizeBytes
	if err := s.saveRecord(ctx, record); err != nil {
		return nil, fmt.Errorf("save initial record: %w", err)
	}
	if err := s.uploadBackupArchive(ctx, record, objectStore, s3Cfg, archivePath); err != nil {
		record.Status = "failed"
		record.ErrorMsg = err.Error()

		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(ctx, record)
		return record, err
	}

	record.Status = "completed"
	record.FinishedAt = time.Now().Format(time.RFC3339)
	if err := s.saveRecord(ctx, record); err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 保存备份记录失败: %v", err)
	}

	return record, nil
}

// StartBackup 异步创建备份，立即返回 running 状态的记录
func (s *BackupService) StartBackup(ctx context.Context, triggeredBy string, expireDays int) (*BackupRecord, error) {
	if s.shuttingDown.Load() {
		return nil, infraerrors.ServiceUnavailable("SERVER_SHUTTING_DOWN", "server is shutting down")
	}

	s.opMu.Lock()
	if s.backingUp {
		s.opMu.Unlock()
		return nil, ErrBackupInProgress
	}
	s.backingUp = true
	s.opMu.Unlock()

	// 初始化阶段出错时自动重置标志
	launched := false
	defer func() {
		if !launched {
			s.opMu.Lock()
			s.backingUp = false
			s.opMu.Unlock()
		}
	}()

	// 在返回前获取 store，避免 goroutine 中配置被修改
	objectStore, prefix, err := s.getOrCreateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("init object store: %w", err)
	}
	s3Cfg, err := s.loadS3Config(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	backupID := uuid.New().String()[:8]
	fileName := fmt.Sprintf("%s_%s.zip", s.dbCfg.DBName, now.Format("20060102_150405"))
	s3Key := buildObjectKey(prefix, fileName)

	var expiresAt string
	if expireDays > 0 {
		expiresAt = now.AddDate(0, 0, expireDays).Format(time.RFC3339)
	}

	record := &BackupRecord{
		ID:          backupID,
		Status:      "running",
		BackupType:  "postgres",
		FileName:    fileName,
		S3Key:       s3Key,
		TriggeredBy: triggeredBy,
		StartedAt:   now.Format(time.RFC3339),
		ExpiresAt:   expiresAt,
		Progress:    "pending",
	}

	if err := s.saveRecord(ctx, record); err != nil {
		return nil, fmt.Errorf("save initial record: %w", err)
	}

	launched = true
	// 在启动 goroutine 前完成拷贝，避免数据竞争
	result := *record

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.opMu.Lock()
			s.backingUp = false
			s.opMu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				logger.LegacyPrintf("service.backup", "[Backup] panic recovered: %v", r)
				record.Status = "failed"
				record.ErrorMsg = fmt.Sprintf("internal panic: %v", r)
				record.Progress = ""
				record.FinishedAt = time.Now().Format(time.RFC3339)
				_ = s.saveRecord(context.Background(), record)
			}
		}()
		s.executeBackup(record, objectStore, s3Cfg)
	}()

	return &result, nil
}

// executeBackup 后台执行备份（独立于 HTTP context）
func (s *BackupService) executeBackup(record *BackupRecord, objectStore BackupObjectStore, s3Cfg *BackupS3Config) {
	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Minute)
	defer cancel()

	// 阶段1: pg_dump -> zip 临时文件
	record.Progress = "dumping"
	_ = s.saveRecord(ctx, record)
	archivePath, sizeBytes, err := s.createCompressedBackupFile(ctx)
	if err != nil {
		record.Status = "failed"
		record.ErrorMsg = err.Error()
		record.Progress = ""
		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(context.Background(), record)
		return
	}
	defer func() { _ = cleanupBackupFiles(archivePath) }()
	record.SizeBytes = sizeBytes

	// 阶段2: 单对象或分卷上传
	record.Progress = "uploading"
	_ = s.saveRecord(ctx, record)
	if err := s.uploadBackupArchive(ctx, record, objectStore, s3Cfg, archivePath); err != nil {
		record.Status = "failed"
		record.ErrorMsg = err.Error()

		record.Progress = ""
		record.FinishedAt = time.Now().Format(time.RFC3339)
		_ = s.saveRecord(context.Background(), record)
		return
	}

	record.SizeBytes = sizeBytes
	record.Status = "completed"
	record.Progress = ""
	record.FinishedAt = time.Now().Format(time.RFC3339)
	if err := s.saveRecord(context.Background(), record); err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 保存备份记录失败: %v", err)
	}
}

func (s *BackupService) createCompressedBackupFile(ctx context.Context) (string, int64, error) {
	dumpReader, err := s.dumper.Dump(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("pg_dump: %w", err)
	}
	archive, err := os.CreateTemp("", "sub2api-backup-*.zip")
	if err != nil {
		_ = dumpReader.Close()
		return "", 0, fmt.Errorf("create backup archive: %w", err)
	}
	archivePath := archive.Name()

	zipWriter := zip.NewWriter(archive)
	entry, copyErr := zipWriter.Create(s.dbCfg.DBName + ".sql")
	if copyErr == nil {
		_, copyErr = io.Copy(entry, dumpReader)
	}
	if closeErr := zipWriter.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if closeErr := dumpReader.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if closeErr := archive.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = cleanupBackupFiles(archivePath)
		return "", 0, fmt.Errorf("zip/dump failed: %w", copyErr)
	}

	info, err := os.Stat(archivePath)
	if err != nil {
		_ = cleanupBackupFiles(archivePath)
		return "", 0, fmt.Errorf("stat backup archive: %w", err)
	}
	return archivePath, info.Size(), nil
}

func (s *BackupService) uploadBackupArchive(ctx context.Context, record *BackupRecord, objectStore BackupObjectStore, cfg *BackupS3Config, archivePath string) error {
	info, err := os.Stat(archivePath)
	if err != nil {
		return fmt.Errorf("stat backup archive: %w", err)
	}
	partSize := s.partSizeBytes
	if partSize <= 0 {
		partSize = defaultBackupPartSizeBytes
	}
	if info.Size() <= partSize || cfg == nil {
		if _, err := objectStore.UploadFile(ctx, record.S3Key, archivePath, "application/zip"); err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), backupObjectCleanupTimeout)
			cleanupErr := deleteBackupObjectKeys(cleanupCtx, objectStore, record)
			cleanupCancel()
			return errors.Join(fmt.Errorf("backup upload: %w", err), cleanupErr)
		}
		record.Parts = nil
		return nil
	}

	localParts, err := splitBackupFile(archivePath, partSize)
	if err != nil {
		return fmt.Errorf("split backup archive: %w", err)
	}
	defer func() {
		paths := make([]string, 0, len(localParts))
		for _, part := range localParts {
			paths = append(paths, part.Path)
		}
		_ = cleanupBackupFiles(paths...)
	}()
	record.S3Key = ""
	record.Parts = make([]BackupPart, 0, len(localParts))
	partRoot := strings.TrimRight(s.buildS3Key(cfg, record.ID), "/")
	for _, part := range localParts {
		record.Parts = append(record.Parts, BackupPart{
			Index:     part.Index,
			S3Key:     s.buildBackupPartKey(partRoot, part.Index),
			SizeBytes: part.SizeBytes,
			SHA256:    part.SHA256,
		})
	}
	if err := s.saveRecord(ctx, record); err != nil {
		return fmt.Errorf("save split backup plan: %w", err)
	}
	for i, part := range localParts {
		if _, err := objectStore.UploadFile(ctx, record.Parts[i].S3Key, part.Path, "application/octet-stream"); err != nil {
			// PUT 可能已经在对象存储端成功、但客户端因超时收到错误；
			// 因此失败时清理整份分卷计划，而不只清理此前返回成功的卷。
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), backupObjectCleanupTimeout)
			cleanupErr := deleteBackupObjectKeys(cleanupCtx, objectStore, record)
			cleanupCancel()
			return errors.Join(fmt.Errorf("upload backup part %d: %w", part.Index, err), cleanupErr)
		}
	}
	return nil
}

func (s *BackupService) buildBackupPartKey(root string, index int) string {
	return fmt.Sprintf("%s/payload.part-%06d", strings.TrimRight(root, "/"), index)
}

func backupUploadErrorMessage(uploadErr, archiveErr error) string {
	message := fmt.Sprintf("backup upload failed: %v", uploadErr)
	if archiveErr != nil {
		message += fmt.Sprintf("; zip/dump also failed: %v", archiveErr)
	}
	return message
}

// writeBackupZIP packages the pg_dump stream into a single SQL entry without
// buffering the dump in memory.
func writeBackupZIP(pw *io.PipeWriter, dumpReader io.ReadCloser, sqlFileName string, done chan<- error) {
	var archiveErr error
	defer func() {
		_ = dumpReader.Close()
		if r := recover(); r != nil {
			archiveErr = fmt.Errorf("zip goroutine panic: %v", r)
			_ = pw.CloseWithError(archiveErr)
		}
		done <- archiveErr
	}()

	zipWriter := zip.NewWriter(pw)
	entry, err := zipWriter.Create(sqlFileName)
	if err == nil {
		_, err = io.Copy(entry, dumpReader)
	}
	if closeErr := dumpReader.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if closeErr := zipWriter.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		archiveErr = err
		_ = pw.CloseWithError(err)
		return
	}
	archiveErr = pw.Close()
}

// RestoreBackup 从 S3 下载备份并流式恢复到数据库
func (s *BackupService) RestoreBackup(ctx context.Context, backupID string) error {
	s.opMu.Lock()
	if s.restoring {
		s.opMu.Unlock()
		return ErrRestoreInProgress
	}
	s.restoring = true
	s.opMu.Unlock()
	defer func() {
		s.opMu.Lock()
		s.restoring = false
		s.opMu.Unlock()
	}()

	record, err := s.GetBackupRecord(ctx, backupID)
	if err != nil {
		return err
	}
	if record.Status != "completed" {
		return infraerrors.BadRequest("BACKUP_NOT_COMPLETED", "can only restore from a completed backup")
	}

	objectStore, _, err := s.getOrCreateStore(ctx)
	if err != nil {
		return err
	}

	return s.restoreBackupData(ctx, record, objectStore)
}

// StartRestore 异步恢复备份，立即返回
func (s *BackupService) StartRestore(ctx context.Context, backupID string) (*BackupRecord, error) {
	if s.shuttingDown.Load() {
		return nil, infraerrors.ServiceUnavailable("SERVER_SHUTTING_DOWN", "server is shutting down")
	}

	s.opMu.Lock()
	if s.restoring {
		s.opMu.Unlock()
		return nil, ErrRestoreInProgress
	}
	s.restoring = true
	s.opMu.Unlock()

	// 初始化阶段出错时自动重置标志
	launched := false
	defer func() {
		if !launched {
			s.opMu.Lock()
			s.restoring = false
			s.opMu.Unlock()
		}
	}()

	record, err := s.GetBackupRecord(ctx, backupID)
	if err != nil {
		return nil, err
	}
	if record.Status != "completed" {
		return nil, infraerrors.BadRequest("BACKUP_NOT_COMPLETED", "can only restore from a completed backup")
	}

	objectStore, _, err := s.getOrCreateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("init object store: %w", err)
	}

	record.RestoreStatus = "running"
	_ = s.saveRecord(ctx, record)

	launched = true
	result := *record

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.opMu.Lock()
			s.restoring = false
			s.opMu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				logger.LegacyPrintf("service.backup", "[Backup] restore panic recovered: %v", r)
				record.RestoreStatus = "failed"
				record.RestoreError = fmt.Sprintf("internal panic: %v", r)
				_ = s.saveRecord(context.Background(), record)
			}
		}()
		s.executeRestore(record, objectStore)
	}()

	return &result, nil
}

// executeRestore 后台执行恢复
func (s *BackupService) executeRestore(record *BackupRecord, objectStore BackupObjectStore) {
	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Minute)
	defer cancel()

	if err := s.restoreBackupData(ctx, record, objectStore); err != nil {
		record.RestoreStatus = "failed"
		record.RestoreError = err.Error()
		_ = s.saveRecord(context.Background(), record)
		return
	}

	record.RestoreStatus = "completed"
	record.RestoredAt = time.Now().Format(time.RFC3339)
	if err := s.saveRecord(context.Background(), record); err != nil {
		logger.LegacyPrintf("service.backup", "[Backup] 保存恢复记录失败: %v", err)
	}
}

func (s *BackupService) restoreBackupData(ctx context.Context, record *BackupRecord, objectStore BackupObjectStore) error {
	if len(record.Parts) > 0 {
		archivePath, err := s.downloadBackupParts(ctx, objectStore, record.Parts)
		if err != nil {
			return err
		}
		defer func() { _ = cleanupBackupFiles(archivePath) }()
		return s.restoreArchive(ctx, archivePath)
	}
	if record.S3Key == "" {
		return errors.New("backup object key is empty")
	}
	body, err := objectStore.Download(ctx, record.S3Key)
	if err != nil {
		return fmt.Errorf("S3 download failed: %w", err)
	}
	defer func() { _ = body.Close() }()

	payload, cleanup, err := openBackupPayload(body, record.FileName)
	if err != nil {
		return err
	}
	defer cleanup()
	defer func() { _ = payload.Close() }()

	if err := s.dumper.Restore(ctx, payload); err != nil {
		return fmt.Errorf("pg restore: %w", err)
	}
	return nil
}

// openBackupPayload supports new ZIP backups and keeps existing .sql.gz
// records restorable after the format migration.
func openBackupPayload(body io.Reader, fileName string) (io.ReadCloser, func(), error) {
	lowerName := strings.ToLower(fileName)
	if strings.HasSuffix(lowerName, ".gz") {
		gzReader, err := gzip.NewReader(body)
		if err != nil {
			return nil, func() {}, fmt.Errorf("gzip reader: %w", err)
		}
		return gzReader, func() {}, nil
	}
	if !strings.HasSuffix(lowerName, ".zip") {
		return nil, func() {}, fmt.Errorf("unsupported backup archive format: %s", fileName)
	}

	tmp, err := os.CreateTemp("", "sub2api-backup-*.zip")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create backup archive temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := io.Copy(tmp, body); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stage backup archive: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("rewind backup archive: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stat backup archive: %w", err)
	}

	archive, err := zip.NewReader(tmp, info.Size())
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("zip reader: %w", err)
	}
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name), ".sql") {
			continue
		}
		payload, err := entry.Open()
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("open SQL entry in zip: %w", err)
		}
		return payload, cleanup, nil
	}

	cleanup()
	return nil, func() {}, fmt.Errorf("zip archive contains no SQL entry")
}

func (s *BackupService) downloadBackupParts(ctx context.Context, objectStore BackupObjectStore, parts []BackupPart) (path string, err error) {
	if len(parts) == 0 {
		return "", errors.New("backup parts are empty")
	}
	ordered := append([]BackupPart(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	for i, part := range ordered {
		if part.Index != i+1 || part.S3Key == "" || part.SizeBytes <= 0 {
			return "", fmt.Errorf("invalid backup part metadata at index %d", i+1)
		}
	}

	archive, err := os.CreateTemp("", "sub2api-restore-*.zip")
	if err != nil {
		return "", fmt.Errorf("create restore archive: %w", err)
	}
	path = archive.Name()
	cleanup := func() {
		_ = archive.Close()
		_ = cleanupBackupFiles(path)
	}

	for _, part := range ordered {
		body, downloadErr := objectStore.Download(ctx, part.S3Key)
		if downloadErr != nil {
			cleanup()
			return "", fmt.Errorf("download backup part %d: %w", part.Index, downloadErr)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(archive, hash), body)
		closeErr := body.Close()
		if copyErr != nil {
			cleanup()
			return "", fmt.Errorf("read backup part %d: %w", part.Index, copyErr)
		}
		if closeErr != nil {
			cleanup()
			return "", fmt.Errorf("close backup part %d: %w", part.Index, closeErr)
		}
		if written != part.SizeBytes {
			cleanup()
			return "", fmt.Errorf("backup part %d size mismatch: got %d, want %d", part.Index, written, part.SizeBytes)
		}
		if part.SHA256 != "" && !strings.EqualFold(part.SHA256, hex.EncodeToString(hash.Sum(nil))) {
			cleanup()
			return "", fmt.Errorf("backup part %d checksum mismatch", part.Index)
		}
	}
	if err := archive.Close(); err != nil {
		_ = cleanupBackupFiles(path)
		return "", fmt.Errorf("close restore archive: %w", err)
	}
	return path, nil
}

func (s *BackupService) restoreArchive(ctx context.Context, archivePath string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open restore archive: %w", err)
	}
	defer func() { _ = archive.Close() }()

	info, err := archive.Stat()
	if err != nil {
		return fmt.Errorf("stat restore archive: %w", err)
	}

	if zipArchive, zipErr := zip.NewReader(archive, info.Size()); zipErr == nil {
		for _, entry := range zipArchive.File {
			if entry.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name), ".sql") {
				continue
			}
			payload, openErr := entry.Open()
			if openErr != nil {
				return fmt.Errorf("open SQL entry in zip: %w", openErr)
			}
			restoreErr := s.dumper.Restore(ctx, payload)
			_ = payload.Close()
			if restoreErr != nil {
				return fmt.Errorf("pg restore: %w", restoreErr)
			}
			return nil
		}
		return errors.New("zip archive contains no SQL entry")
	}

	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind restore archive: %w", err)
	}
	gzReader, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()
	if err := s.dumper.Restore(ctx, gzReader); err != nil {
		return fmt.Errorf("pg restore: %w", err)
	}
	return nil
}

// ─── 备份记录管理 ───

func (s *BackupService) ListBackups(ctx context.Context) ([]BackupRecord, error) {
	records, err := s.loadRecords(ctx)
	if err != nil {
		return nil, err
	}
	// 倒序返回（最新在前）
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt > records[j].StartedAt
	})
	return records, nil
}

func (s *BackupService) GetBackupRecord(ctx context.Context, backupID string) (*BackupRecord, error) {
	records, err := s.loadRecords(ctx)
	if err != nil {
		return nil, err
	}
	for i := range records {
		if records[i].ID == backupID {
			return &records[i], nil
		}
	}
	return nil, ErrBackupNotFound
}

func (s *BackupService) DeleteBackup(ctx context.Context, backupID string) error {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	records, err := s.loadRecordsLocked(ctx)
	if err != nil {
		return err
	}

	var found *BackupRecord
	var remaining []BackupRecord
	for i := range records {
		if records[i].ID == backupID {
			found = &records[i]
		} else {
			remaining = append(remaining, records[i])
		}
	}
	if found == nil {
		return ErrBackupNotFound
	}
	if found.Status == "running" {
		// 后台上传仍可能依赖 Parts 计划；删除对象会让随后完成的记录引用失效卷。
		return ErrBackupInProgress
	}

	// 从对象存储删除所有单文件或分卷对象。删除不完整时保留记录，便于重试。
	if err := s.deleteBackupObjects(ctx, found); err != nil {
		return err
	}

	return s.saveRecordsLocked(ctx, remaining)
}

// GetBackupDownloadURL 获取备份文件预签名下载 URL
func (s *BackupService) GetBackupDownloadURL(ctx context.Context, backupID string) (BackupDownloadResponse, error) {
	var download BackupDownloadResponse
	record, err := s.GetBackupRecord(ctx, backupID)
	if err != nil {
		return download, err
	}
	if record.Status != "completed" {
		return download, infraerrors.BadRequest("BACKUP_NOT_COMPLETED", "backup is not completed")
	}

	objectStore, _, err := s.getOrCreateStore(ctx)
	if err != nil {
		return download, err
	}

	if len(record.Parts) > 0 {
		parts := append([]BackupPart(nil), record.Parts...)
		sort.Slice(parts, func(i, j int) bool { return parts[i].Index < parts[j].Index })
		for i, part := range parts {
			if part.Index != i+1 || part.S3Key == "" || part.SizeBytes <= 0 {
				return download, fmt.Errorf("invalid backup part metadata at index %d", i+1)
			}
			url, presignErr := objectStore.PresignURL(ctx, part.S3Key, 1*time.Hour)
			if presignErr != nil {
				return download, fmt.Errorf("presign backup part %d: %w", part.Index, presignErr)
			}
			download.Parts = append(download.Parts, BackupDownloadPart{
				Index:     part.Index,
				SizeBytes: part.SizeBytes,
				URL:       url,
			})
		}
		return download, nil
	}
	if record.S3Key == "" {
		return download, errors.New("backup object key is empty")
	}
	url, err := objectStore.PresignURL(ctx, record.S3Key, 1*time.Hour)
	if err != nil {
		return download, fmt.Errorf("presign url: %w", err)
	}
	download.URL = url
	if url == "" {
		download.URL = fmt.Sprintf("/api/admin/backups/%s/download", backupID)
	}
	return download, nil
}

// StreamDownload 将备份文件内容写入 w，供 WebDAV 后端代理下载使用。
// 新 ZIP 备份按原格式下载；旧 .sql.gz 备份保持兼容，下载为解压后的 SQL。
func (s *BackupService) StreamDownload(ctx context.Context, backupID string, w io.Writer) (fileName string, err error) {
	record, err := s.GetBackupRecord(ctx, backupID)
	if err != nil {
		return "", err
	}
	if record.Status != "completed" {
		return "", infraerrors.BadRequest("BACKUP_NOT_COMPLETED", "backup is not completed")
	}

	objectStore, _, err := s.getOrCreateStore(ctx)
	if err != nil {
		return "", fmt.Errorf("init object store: %w", err)
	}

	body, err := objectStore.Download(ctx, record.S3Key)
	if err != nil {
		return "", fmt.Errorf("download backup: %w", err)
	}
	defer func() { _ = body.Close() }()

	if strings.HasSuffix(strings.ToLower(record.FileName), ".gz") {
		gzReader, err := gzip.NewReader(body)
		if err != nil {
			return "", fmt.Errorf("gzip reader: %w", err)
		}
		defer func() { _ = gzReader.Close() }()
		if _, err := io.Copy(w, gzReader); err != nil {
			return "", fmt.Errorf("stream backup: %w", err)
		}
		return strings.TrimSuffix(record.FileName, ".gz"), nil
	}

	if _, err := io.Copy(w, body); err != nil {
		return "", fmt.Errorf("stream backup: %w", err)
	}
	return record.FileName, nil
}

// ─── 内部方法 ───

func (s *BackupService) loadS3Config(ctx context.Context) (*BackupS3Config, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupS3Config)
	if err != nil || raw == "" {
		return nil, nil //nolint:nilnil // no config is a valid state
	}
	var cfg BackupS3Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, ErrBackupS3ConfigCorrupt
	}
	// 解密 SecretAccessKey
	if cfg.SecretAccessKey != "" {
		decrypted, err := s.encryptor.Decrypt(cfg.SecretAccessKey)
		if err != nil {
			// 兼容未加密的旧数据：如果解密失败，保持原值
			logger.LegacyPrintf("service.backup", "[Backup] S3 SecretAccessKey 解密失败（可能是旧的未加密数据）: %v", err)
		} else {
			cfg.SecretAccessKey = decrypted
		}
	}
	return &cfg, nil
}

func (s *BackupService) loadStorageType(ctx context.Context) (BackupStorageType, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupStorageType)
	if err != nil || raw == "" {
		return BackupStorageTypeS3, nil // 默认 S3，向后兼容
	}
	t := BackupStorageType(raw)
	if t != BackupStorageTypeS3 && t != BackupStorageTypeWebDAV {
		return BackupStorageTypeS3, nil
	}
	return t, nil
}

func (s *BackupService) loadWebDAVConfig(ctx context.Context) (*BackupWebDAVConfig, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupWebDAVConfig)
	if err != nil || raw == "" {
		return nil, nil //nolint:nilnil // no config is a valid state
	}
	var cfg BackupWebDAVConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, ErrBackupWebDAVConfigCorrupt
	}
	// 解密密码
	if cfg.Password != "" {
		decrypted, err := s.encryptor.Decrypt(cfg.Password)
		if err != nil {
			logger.LegacyPrintf("service.backup", "[Backup] WebDAV Password 解密失败（可能是旧的未加密数据）: %v", err)
		} else {
			cfg.Password = decrypted
		}
	}
	return &cfg, nil
}

func (s *BackupService) getOrCreateStore(ctx context.Context) (store BackupObjectStore, prefix string, err error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()

	storageType, err := s.loadStorageType(ctx)
	if err != nil {
		return nil, "", err
	}

	// 命中缓存：类型一致且 store 已初始化
	if s.store != nil && s.storageType == storageType {
		return s.store, s.cachedPrefix(), nil
	}

	switch storageType {
	case BackupStorageTypeWebDAV:
		cfg, err := s.loadWebDAVConfig(ctx)
		if err != nil {
			return nil, "", err
		}
		if cfg == nil || !cfg.IsConfigured() {
			return nil, "", ErrBackupWebDAVNotConfigured
		}
		ws := s.webdavFactory(cfg)
		s.store = ws
		s.storageType = BackupStorageTypeWebDAV
		s.webdavCfg = cfg
		s.s3Cfg = nil
		return ws, buildKeyPrefix(cfg.Prefix), nil

	default: // BackupStorageTypeS3
		cfg, err := s.loadS3Config(ctx)
		if err != nil {
			return nil, "", err
		}
		if cfg == nil || !cfg.IsConfigured() {
			return nil, "", ErrBackupS3NotConfigured
		}
		ss, err := s.storeFactory(ctx, cfg)
		if err != nil {
			return nil, "", err
		}
		s.store = ss
		s.storageType = BackupStorageTypeS3
		s.s3Cfg = cfg
		s.webdavCfg = nil
		return ss, buildKeyPrefix(cfg.Prefix), nil
	}
}

// cachedPrefix returns the prefix from the currently cached config (must be called under storeMu).
func (s *BackupService) cachedPrefix() string {
	if s.storageType == BackupStorageTypeWebDAV && s.webdavCfg != nil {
		return buildKeyPrefix(s.webdavCfg.Prefix)
	}
	if s.s3Cfg != nil {
		return buildKeyPrefix(s.s3Cfg.Prefix)
	}
	return "backups"
}

// buildKeyPrefix normalises a user-supplied prefix, defaulting to "backups".
func buildKeyPrefix(prefix string) string {
	p := strings.TrimRight(prefix, "/")
	if p == "" {
		return "backups"
	}
	return p
}

// buildObjectKey builds the full object storage key for a backup file.
func buildObjectKey(prefix, fileName string) string {
	return fmt.Sprintf("%s/%s/%s", prefix, time.Now().Format("2006/01/02"), fileName)
}

func (s *BackupService) buildS3Key(cfg *BackupS3Config, fileName string) string {
	if cfg == nil {
		return buildObjectKey("backups", fileName)
	}
	return buildObjectKey(buildKeyPrefix(cfg.Prefix), fileName)
}

// loadRecords 加载备份记录，区分"无数据"和"数据损坏"
func (s *BackupService) loadRecords(ctx context.Context) ([]BackupRecord, error) {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()
	return s.loadRecordsLocked(ctx)
}

// loadRecordsLocked 在已持有 recordsMu 锁的情况下加载记录
func (s *BackupService) loadRecordsLocked(ctx context.Context) ([]BackupRecord, error) {
	raw, err := s.settingRepo.GetValue(ctx, settingKeyBackupRecords)
	if err != nil || raw == "" {
		return nil, nil //nolint:nilnil // no records is a valid state
	}
	var records []BackupRecord
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return nil, ErrBackupRecordsCorrupt
	}
	return records, nil
}

// saveRecordsLocked 在已持有 recordsMu 锁的情况下保存记录
func (s *BackupService) saveRecordsLocked(ctx context.Context, records []BackupRecord) error {
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, settingKeyBackupRecords, string(data))
}

// saveRecord 保存单条记录（带互斥锁保护）
func (s *BackupService) saveRecord(ctx context.Context, record *BackupRecord) error {
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	records, _ := s.loadRecordsLocked(ctx)

	// 更新已有记录或追加
	found := false
	for i := range records {
		if records[i].ID == record.ID {
			records[i] = *record
			found = true
			break
		}
	}
	if !found {
		records = append(records, *record)
	}

	// 限制记录数量
	if len(records) > maxBackupRecords {
		records = records[len(records)-maxBackupRecords:]
	}

	return s.saveRecordsLocked(ctx, records)
}

func (s *BackupService) cleanupOldBackups(ctx context.Context, schedule *BackupScheduleConfig) error {
	if schedule == nil {
		return nil
	}

	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()

	records, err := s.loadRecordsLocked(ctx)
	if err != nil {
		return err
	}

	// 按时间倒序
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt > records[j].StartedAt
	})

	var toDelete []BackupRecord
	var toKeep []BackupRecord

	for i, r := range records {
		shouldDelete := false

		// 按保留份数清理
		if schedule.RetainCount > 0 && i >= schedule.RetainCount {
			shouldDelete = true
		}

		// 按保留天数清理
		if schedule.RetainDays > 0 && r.StartedAt != "" {
			startedAt, err := time.Parse(time.RFC3339, r.StartedAt)
			if err == nil && time.Since(startedAt) > time.Duration(schedule.RetainDays)*24*time.Hour {
				shouldDelete = true
			}
		}

		if shouldDelete && r.Status == "completed" {
			toDelete = append(toDelete, r)
		} else {
			toKeep = append(toKeep, r)
		}
	}

	var cleanupErrs []error
	deletedCount := 0
	for _, r := range toDelete {
		if err := s.deleteBackupObjects(ctx, &r); err != nil {
			// 对象删除失败时保留记录，避免丢失后续重试所需的 key。
			toKeep = append(toKeep, r)
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup backup %s: %w", r.ID, err))
			continue
		}
		deletedCount++
	}

	if len(toDelete) > 0 {
		if err := s.saveRecordsLocked(ctx, toKeep); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("save backup records after cleanup: %w", err))
		}
		if deletedCount > 0 {
			logger.LegacyPrintf("service.backup", "[Backup] 自动清理了 %d 个过期备份", deletedCount)
		}
		return errors.Join(cleanupErrs...)
	}
	return nil
}

// backupObjectKeys 返回一条备份记录关联的全部对象 key。
// 新记录使用 Parts，旧记录使用 S3Key；两者同时存在时也全部返回，便于清理异常残留对象。
func backupObjectKeys(record *BackupRecord) []string {
	if record == nil {
		return nil
	}
	keys := make([]string, 0, len(record.Parts)+1)
	seen := make(map[string]struct{}, len(record.Parts)+1)
	appendKey := func(key string) {
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	appendKey(record.S3Key)
	parts := append([]BackupPart(nil), record.Parts...)
	sort.Slice(parts, func(i, j int) bool { return parts[i].Index < parts[j].Index })
	for _, part := range parts {
		appendKey(part.S3Key)
	}
	return keys
}

// deleteBackupObjects 尝试删除记录关联的所有对象，并聚合删除错误。
func (s *BackupService) deleteBackupObjects(ctx context.Context, record *BackupRecord) error {
	if len(backupObjectKeys(record)) == 0 {
		return nil
	}
	storageType, err := s.loadStorageType(ctx)
	if err != nil {
		return err
	}
	if storageType == BackupStorageTypeS3 {
		s3Cfg, cfgErr := s.loadS3Config(ctx)
		if cfgErr != nil {
			return cfgErr
		}
		if s3Cfg == nil || !s3Cfg.IsConfigured() {
			// 兼容没有配置对象存储的旧记录：记录仍可被删除。
			return nil
		}
	}
	objectStore, _, err := s.getOrCreateStore(ctx)
	if err != nil {
		return nil // 存储未配置时静默忽略
	}
	return deleteBackupObjectKeys(ctx, objectStore, record)
}

func deleteBackupObjectKeys(ctx context.Context, objectStore BackupObjectStore, record *BackupRecord) error {
	keys := backupObjectKeys(record)
	if len(keys) == 0 {
		return nil
	}
	var errs []error
	for _, key := range keys {
		if deleteErr := objectStore.Delete(ctx, key); deleteErr != nil {
			errs = append(errs, fmt.Errorf("delete backup object %q: %w", key, deleteErr))
		}
	}
	return errors.Join(errs...)
}
