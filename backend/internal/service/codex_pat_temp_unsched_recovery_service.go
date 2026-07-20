package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// CodexPATTempUnschedRecoveryRepository is the narrow persistence contract
// required by the Codex PAT temporary-state recovery worker.
type CodexPATTempUnschedRecoveryRepository interface {
	ListByPlatform(ctx context.Context, platform string) ([]Account, error)
	ClearTempUnschedulable(ctx context.Context, id int64) error
}

type CodexPATTempUnschedRecoverySettingsProvider interface {
	GetCodexPATTempUnschedRecoverySettings(ctx context.Context) (*CodexPATTempUnschedRecoverySettings, error)
}

// CodexPATTempUnschedRecoveryService periodically restores active Codex PAT
// accounts from temporary-unschedulable state. PATs cannot be token-refreshed,
// so this is intentionally opt-in and never changes manual or permanent state.
type CodexPATTempUnschedRecoveryService struct {
	accountRepo    CodexPATTempUnschedRecoveryRepository
	tempUnsched    TempUnschedCache
	runtimeBlocker AccountRuntimeBlocker
	settings       CodexPATTempUnschedRecoverySettingsProvider
	fallback       CodexPATTempUnschedRecoverySettings

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewCodexPATTempUnschedRecoveryService(
	accountRepo CodexPATTempUnschedRecoveryRepository,
	tempUnsched TempUnschedCache,
	runtimeBlocker AccountRuntimeBlocker,
	cfg *config.Config,
	settingsProviders ...CodexPATTempUnschedRecoverySettingsProvider,
) *CodexPATTempUnschedRecoveryService {
	fallback := CodexPATTempUnschedRecoverySettings{IntervalSeconds: 30}
	if cfg != nil {
		fallback.Enabled = cfg.RateLimit.CodexPATTempUnschedRecoveryEnabled
		fallback.IntervalSeconds = cfg.RateLimit.CodexPATTempUnschedRecoveryIntervalSeconds
		if fallback.IntervalSeconds < 1 {
			fallback.IntervalSeconds = 30
		}
	}
	var settings CodexPATTempUnschedRecoverySettingsProvider
	if len(settingsProviders) > 0 {
		settings = settingsProviders[0]
	}
	return &CodexPATTempUnschedRecoveryService{
		accountRepo:    accountRepo,
		tempUnsched:    tempUnsched,
		runtimeBlocker: runtimeBlocker,
		settings:       settings,
		fallback:       fallback,
		stopCh:         make(chan struct{}),
	}
}

func (s *CodexPATTempUnschedRecoveryService) Start() {
	if s == nil || s.accountRepo == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			settings := s.currentSettings()
			if settings.Enabled {
				s.runOnce()
			}
			wait := time.NewTimer(time.Duration(settings.IntervalSeconds) * time.Second)
			select {
			case <-s.stopCh:
				if !wait.Stop() {
					select {
					case <-wait.C:
					default:
					}
				}
				return
			case <-wait.C:
			}
		}
	}()
	slog.Info("codex_pat_temp_unsched_recovery_started", "fallback_interval_seconds", s.fallback.IntervalSeconds)
}

func (s *CodexPATTempUnschedRecoveryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

func (s *CodexPATTempUnschedRecoveryService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		slog.Warn("codex_pat_temp_unsched_recovery_list_failed", "error", err)
		return
	}

	now := time.Now()
	recovered := 0
	for i := range accounts {
		account := &accounts[i]
		if !account.IsOpenAIPersonalAccessToken() || account.TempUnschedulableUntil == nil || !now.Before(*account.TempUnschedulableUntil) {
			continue
		}

		if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
			slog.Warn("codex_pat_temp_unsched_recovery_clear_failed", "account_id", account.ID, "error", err)
			continue
		}
		if s.tempUnsched != nil {
			if err := s.tempUnsched.DeleteTempUnsched(ctx, account.ID); err != nil {
				slog.Warn("codex_pat_temp_unsched_recovery_cache_clear_failed", "account_id", account.ID, "error", err)
			}
		}
		if s.runtimeBlocker != nil {
			s.runtimeBlocker.ClearAccountSchedulingBlock(account.ID)
		}
		recovered++
	}

	if recovered > 0 {
		slog.Info("codex_pat_temp_unsched_recovery_completed", "recovered", recovered)
	}
}

func (s *CodexPATTempUnschedRecoveryService) currentSettings() CodexPATTempUnschedRecoverySettings {
	if s == nil {
		return CodexPATTempUnschedRecoverySettings{IntervalSeconds: 30}
	}
	if s.settings == nil {
		return s.fallback
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	settings, err := s.settings.GetCodexPATTempUnschedRecoverySettings(ctx)
	if err != nil || settings == nil {
		if err != nil {
			slog.Warn("codex_pat_temp_unsched_recovery_settings_failed", "error", err)
		}
		return s.fallback
	}
	resolved := *settings
	if resolved.IntervalSeconds < 1 {
		resolved.IntervalSeconds = s.fallback.IntervalSeconds
	}
	return resolved
}
