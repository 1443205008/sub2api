//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type bulkUserRepoStub struct {
	users         map[int64]*User
	deleteErrByID map[int64]error
	updateErrByID map[int64]error
	deletedIDs    []int64
	updatedUsers  []*User
}

func cloneTestUser(user *User) *User {
	if user == nil {
		return nil
	}
	cloned := *user
	if user.AllowedGroups != nil {
		cloned.AllowedGroups = append([]int64{}, user.AllowedGroups...)
	}
	if user.GroupRates != nil {
		cloned.GroupRates = make(map[int64]float64, len(user.GroupRates))
		for groupID, rate := range user.GroupRates {
			cloned.GroupRates[groupID] = rate
		}
	}
	return &cloned
}

func (s *bulkUserRepoStub) Create(context.Context, *User) error {
	panic("unexpected Create call")
}

func (s *bulkUserRepoStub) GetByID(_ context.Context, id int64) (*User, error) {
	user, ok := s.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneTestUser(user), nil
}

func (s *bulkUserRepoStub) GetByEmail(context.Context, string) (*User, error) {
	panic("unexpected GetByEmail call")
}

func (s *bulkUserRepoStub) GetFirstAdmin(context.Context) (*User, error) {
	panic("unexpected GetFirstAdmin call")
}

func (s *bulkUserRepoStub) Update(_ context.Context, user *User) error {
	if err, ok := s.updateErrByID[user.ID]; ok {
		return err
	}
	cloned := cloneTestUser(user)
	s.users[user.ID] = cloned
	s.updatedUsers = append(s.updatedUsers, cloned)
	return nil
}

func (s *bulkUserRepoStub) Delete(_ context.Context, id int64) error {
	if err, ok := s.deleteErrByID[id]; ok {
		return err
	}
	if _, ok := s.users[id]; !ok {
		return ErrUserNotFound
	}
	delete(s.users, id)
	s.deletedIDs = append(s.deletedIDs, id)
	return nil
}

func (s *bulkUserRepoStub) List(context.Context, pagination.PaginationParams) ([]User, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}

func (s *bulkUserRepoStub) ListWithFilters(context.Context, pagination.PaginationParams, UserListFilters) ([]User, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}

func (s *bulkUserRepoStub) UpdateBalance(context.Context, int64, float64) error {
	panic("unexpected UpdateBalance call")
}

func (s *bulkUserRepoStub) DeductBalance(context.Context, int64, float64) error {
	panic("unexpected DeductBalance call")
}

func (s *bulkUserRepoStub) UpdateConcurrency(context.Context, int64, int) error {
	panic("unexpected UpdateConcurrency call")
}

func (s *bulkUserRepoStub) ExistsByEmail(context.Context, string) (bool, error) {
	panic("unexpected ExistsByEmail call")
}

func (s *bulkUserRepoStub) RemoveGroupFromAllowedGroups(context.Context, int64) (int64, error) {
	panic("unexpected RemoveGroupFromAllowedGroups call")
}

func (s *bulkUserRepoStub) AddGroupToAllowedGroups(context.Context, int64, int64) error {
	panic("unexpected AddGroupToAllowedGroups call")
}

func (s *bulkUserRepoStub) RemoveGroupFromUserAllowedGroups(context.Context, int64, int64) error {
	panic("unexpected RemoveGroupFromUserAllowedGroups call")
}

func (s *bulkUserRepoStub) UpdateTotpSecret(context.Context, int64, *string) error {
	panic("unexpected UpdateTotpSecret call")
}

func (s *bulkUserRepoStub) EnableTotp(context.Context, int64) error {
	panic("unexpected EnableTotp call")
}

func (s *bulkUserRepoStub) DisableTotp(context.Context, int64) error {
	panic("unexpected DisableTotp call")
}

func TestAdminService_BulkManageUsers_DeletePartial(t *testing.T) {
	repo := &bulkUserRepoStub{
		users: map[int64]*User{
			1: {ID: 1, Role: RoleUser, Status: StatusActive},
			2: {ID: 2, Role: RoleAdmin, Status: StatusActive},
		},
	}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		userRepo:             repo,
		authCacheInvalidator: invalidator,
	}

	result, err := svc.BulkManageUsers(context.Background(), &BulkManageUsersInput{
		UserIDs: []int64{1, 2},
		Action:  BulkUserActionDelete,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, []int64{1}, result.SuccessIDs)
	require.Equal(t, []int64{2}, result.FailedIDs)
	require.Len(t, result.Results, 2)
	require.True(t, result.Results[0].Success)
	require.False(t, result.Results[1].Success)
	require.Contains(t, result.Results[1].Error, "cannot delete admin user")
	require.Equal(t, []int64{1}, repo.deletedIDs)
	require.Equal(t, []int64{1}, invalidator.userIDs)
}

func TestAdminService_BulkManageUsers_SubtractBalancePartial(t *testing.T) {
	repo := &bulkUserRepoStub{
		users: map[int64]*User{
			1: {ID: 1, Role: RoleUser, Status: StatusActive, Balance: 10},
			2: {ID: 2, Role: RoleUser, Status: StatusActive, Balance: 1},
		},
	}
	redeemRepo := &balanceRedeemRepoStub{redeemRepoStub: &redeemRepoStub{}}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		userRepo:             repo,
		redeemCodeRepo:       redeemRepo,
		authCacheInvalidator: invalidator,
	}

	amount := 2.0
	result, err := svc.BulkManageUsers(context.Background(), &BulkManageUsersInput{
		UserIDs: []int64{1, 2},
		Action:  BulkUserActionSubtractBalance,
		Amount:  &amount,
		Notes:   "bulk refund",
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, 1, result.Failed)
	require.Equal(t, []int64{1}, result.SuccessIDs)
	require.Equal(t, []int64{2}, result.FailedIDs)
	require.Len(t, redeemRepo.created, 1)
	require.Equal(t, int64(1), *redeemRepo.created[0].UsedBy)
	require.Equal(t, 8.0, repo.users[1].Balance)
	require.Equal(t, 1.0, repo.users[2].Balance)
	require.Equal(t, []int64{1}, invalidator.userIDs)
	require.Contains(t, result.Results[1].Error, "balance cannot be negative")
}
