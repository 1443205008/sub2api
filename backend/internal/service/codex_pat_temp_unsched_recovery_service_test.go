package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type codexPATRecoveryRepoStub struct {
	accounts  []Account
	clearedID []int64
	clearErr  error
}

func (r *codexPATRecoveryRepoStub) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	if platform != PlatformOpenAI {
		return nil, errors.New("unexpected platform")
	}
	return r.accounts, nil
}

func (r *codexPATRecoveryRepoStub) ClearTempUnschedulable(_ context.Context, id int64) error {
	if r.clearErr != nil {
		return r.clearErr
	}
	r.clearedID = append(r.clearedID, id)
	return nil
}

type codexPATRecoveryCacheStub struct{ deletedID []int64 }

func (*codexPATRecoveryCacheStub) SetTempUnsched(context.Context, int64, *TempUnschedState) error {
	return nil
}
func (*codexPATRecoveryCacheStub) GetTempUnsched(context.Context, int64) (*TempUnschedState, error) {
	return nil, nil
}
func (c *codexPATRecoveryCacheStub) DeleteTempUnsched(_ context.Context, id int64) error {
	c.deletedID = append(c.deletedID, id)
	return nil
}

type codexPATRecoveryRuntimeBlockerStub struct{ clearedID []int64 }

func (*codexPATRecoveryRuntimeBlockerStub) BlockAccountScheduling(*Account, time.Time, string) {}
func (b *codexPATRecoveryRuntimeBlockerStub) ClearAccountSchedulingBlock(id int64) {
	b.clearedID = append(b.clearedID, id)
}

type codexPATRecoverySettingsStub struct {
	settings *CodexPATTempUnschedRecoverySettings
	err      error
}

func (s *codexPATRecoverySettingsStub) GetCodexPATTempUnschedRecoverySettings(context.Context) (*CodexPATTempUnschedRecoverySettings, error) {
	return s.settings, s.err
}

func TestCodexPATTempUnschedRecoveryServiceRunOnce(t *testing.T) {
	now := time.Now()
	repo := &codexPATRecoveryRepoStub{accounts: []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}, TempUnschedulableUntil: codexPATRecoveryTimePtr(now.Add(time.Minute))},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": "oauth"}, TempUnschedulableUntil: codexPATRecoveryTimePtr(now.Add(time.Minute))},
		{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}, TempUnschedulableUntil: codexPATRecoveryTimePtr(now.Add(-time.Minute))},
		{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}},
	}}
	cache := &codexPATRecoveryCacheStub{}
	runtimeBlocker := &codexPATRecoveryRuntimeBlockerStub{}
	svc := NewCodexPATTempUnschedRecoveryService(repo, cache, runtimeBlocker, &config.Config{})

	svc.runOnce()

	require.Equal(t, []int64{1}, repo.clearedID)
	require.Equal(t, []int64{1}, cache.deletedID)
	require.Equal(t, []int64{1}, runtimeBlocker.clearedID)
}

func TestCodexPATTempUnschedRecoveryServiceDoesNotClearCachesAfterDatabaseFailure(t *testing.T) {
	repo := &codexPATRecoveryRepoStub{
		accounts: []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}, TempUnschedulableUntil: codexPATRecoveryTimePtr(time.Now().Add(time.Minute))}},
		clearErr: errors.New("database unavailable"),
	}
	cache := &codexPATRecoveryCacheStub{}
	runtimeBlocker := &codexPATRecoveryRuntimeBlockerStub{}
	svc := NewCodexPATTempUnschedRecoveryService(repo, cache, runtimeBlocker, &config.Config{})

	svc.runOnce()

	require.Empty(t, cache.deletedID)
	require.Empty(t, runtimeBlocker.clearedID)
}

func TestCodexPATTempUnschedRecoveryServiceUsesRuntimeSettings(t *testing.T) {
	settings := &codexPATRecoverySettingsStub{settings: &CodexPATTempUnschedRecoverySettings{
		Enabled:         true,
		IntervalSeconds: 7,
	}}
	svc := NewCodexPATTempUnschedRecoveryService(&codexPATRecoveryRepoStub{}, nil, nil, &config.Config{}, settings)

	actual := svc.currentSettings()

	require.True(t, actual.Enabled)
	require.Equal(t, 7, actual.IntervalSeconds)
}

func codexPATRecoveryTimePtr(t time.Time) *time.Time { return &t }
