package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/ent/redeemcode"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

var (
	ErrRedeemCodeNotFound  = infraerrors.NotFound("REDEEM_CODE_NOT_FOUND", "redeem code not found")
	ErrRedeemCodeUsed      = infraerrors.Conflict("REDEEM_CODE_USED", "redeem code already used")
	ErrInsufficientBalance = infraerrors.BadRequest("INSUFFICIENT_BALANCE", "insufficient balance")
	ErrRedeemRateLimited   = infraerrors.TooManyRequests("REDEEM_RATE_LIMITED", "too many failed attempts, please try again later")
	ErrRedeemCodeLocked    = infraerrors.Conflict("REDEEM_CODE_LOCKED", "redeem code is being processed, please try again")
)

const (
	redeemMaxErrorsPerHour      = 20
	redeemRateLimitDuration     = time.Hour
	redeemLockDuration          = 10 * time.Second // 锁超时时间，防止死锁
	defaultInviteOverviewLimit  = 10
	maxInviteOverviewLimit      = 50
	defaultInviteRecordsPageSize = 10
	maxInviteRecordsPageSize     = 100
	defaultInviteRankingPageSize = 20
	maxInviteRankingPageSize     = 100
	inviteCodeIssuerNotePrefix  = "invite_issuer_user_id:"
	inviteUsageNotePrefix       = "invite_usage_issuer_user_id:"
	inviteCashbackNotePrefix    = "invite_cashback_from_user_id:"
	inviteCashbackAmountDecimal = 1e8
)

const (
	inviteStatusAll          = ""
	inviteStatusRecharged    = "recharged"
	inviteStatusNotRecharged = "not_recharged"

	inviteSortOrderAsc  = "asc"
	inviteSortOrderDesc = "desc"

	inviteRecordSortRegisteredAt  = "registered_at"
	inviteRecordSortLastCashback  = "last_cashback_at"
	inviteRecordSortTotalCashback = "total_cashback"

	inviteRankingSortInvitedUsers = "invited_users"
	inviteRankingSortTotalCashback = "total_cashback"
	inviteRankingSortLastInviteAt = "last_invite_at"
	inviteRankingSortLastCashback = "last_cashback_at"
)

// RedeemCache defines cache operations for redeem service
type RedeemCache interface {
	GetRedeemAttemptCount(ctx context.Context, userID int64) (int, error)
	IncrementRedeemAttemptCount(ctx context.Context, userID int64) error

	AcquireRedeemLock(ctx context.Context, code string, ttl time.Duration) (bool, error)
	ReleaseRedeemLock(ctx context.Context, code string) error
}

type RedeemCodeRepository interface {
	Create(ctx context.Context, code *RedeemCode) error
	CreateBatch(ctx context.Context, codes []RedeemCode) error
	GetByID(ctx context.Context, id int64) (*RedeemCode, error)
	GetByCode(ctx context.Context, code string) (*RedeemCode, error)
	Update(ctx context.Context, code *RedeemCode) error
	Delete(ctx context.Context, id int64) error
	Use(ctx context.Context, id, userID int64) error

	List(ctx context.Context, params pagination.PaginationParams) ([]RedeemCode, *pagination.PaginationResult, error)
	ListWithFilters(ctx context.Context, params pagination.PaginationParams, codeType, status, search string) ([]RedeemCode, *pagination.PaginationResult, error)
	ListByUser(ctx context.Context, userID int64, limit int) ([]RedeemCode, error)
	// ListByUserPaginated returns paginated balance/concurrency history for a specific user.
	// codeType filter is optional - pass empty string to return all types.
	ListByUserPaginated(ctx context.Context, userID int64, params pagination.PaginationParams, codeType string) ([]RedeemCode, *pagination.PaginationResult, error)
	// SumPositiveBalanceByUser returns the total recharged amount (sum of positive balance values) for a user.
	SumPositiveBalanceByUser(ctx context.Context, userID int64) (float64, error)
}

// GenerateCodesRequest 生成兑换码请求
type GenerateCodesRequest struct {
	Count int     `json:"count"`
	Value float64 `json:"value"`
	Type  string  `json:"type"`
}

// RedeemCodeResponse 兑换码响应
type RedeemCodeResponse struct {
	Code      string    `json:"code"`
	Value     float64   `json:"value"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type InviteCodeItem struct {
	Code      string     `json:"code"`
	Status    string     `json:"status"`
	UsedBy    *int64     `json:"used_by"`
	UsedAt    *time.Time `json:"used_at"`
	CreatedAt time.Time  `json:"created_at"`
}

type InviteOverview struct {
	CashbackRate  float64          `json:"cashback_rate"`
	InvitedUsers  int64            `json:"invited_users"`
	TotalCashback float64          `json:"total_cashback"`
	Codes         []InviteCodeItem `json:"codes"`
	Records       InviteRecordList `json:"records"`
}

type InviteRecord struct {
	InvitedUserID          int64      `json:"invited_user_id"`
	InvitedUserEmailMasked string     `json:"invited_user_email_masked"`
	RegisteredAt           time.Time  `json:"registered_at"`
	InviteUsedAt           *time.Time `json:"invite_used_at,omitempty"`
	TotalCashback          float64    `json:"total_cashback"`
	CashbackCount          int        `json:"cashback_count"`
	LastCashbackAt         *time.Time `json:"last_cashback_at,omitempty"`
}

type InviteRecordList struct {
	Items     []InviteRecord `json:"items"`
	Total     int64          `json:"total"`
	Page      int            `json:"page"`
	PageSize  int            `json:"page_size"`
	Pages     int            `json:"pages"`
	SortBy    string         `json:"sort_by"`
	SortOrder string         `json:"sort_order"`
	Status    string         `json:"status"`
}

type InviteOverviewQuery struct {
	CodesLimit       int
	RecordsPage      int
	RecordsPageSize  int
	RecordsSortBy    string
	RecordsSortOrder string
	RecordsStatus    string
	RecordsDateFrom  *time.Time
	RecordsDateTo    *time.Time
}

type InviteLeaderboardItem struct {
	InviterUserID  int64      `json:"inviter_user_id"`
	InviterEmail   string     `json:"inviter_email"`
	InviterUsername string    `json:"inviter_username"`
	InvitedUsers   int64      `json:"invited_users"`
	TotalCashback  float64    `json:"total_cashback"`
	CashbackCount  int        `json:"cashback_count"`
	LastInviteAt   *time.Time `json:"last_invite_at,omitempty"`
	LastCashbackAt *time.Time `json:"last_cashback_at,omitempty"`
}

type InviteLeaderboardPage struct {
	Items     []InviteLeaderboardItem `json:"items"`
	Total     int64                   `json:"total"`
	Page      int                     `json:"page"`
	PageSize  int                     `json:"page_size"`
	Pages     int                     `json:"pages"`
	SortBy    string                  `json:"sort_by"`
	SortOrder string                  `json:"sort_order"`
	Status    string                  `json:"status"`
	Search    string                  `json:"search"`
}

type InviteLeaderboardQuery struct {
	Page      int
	PageSize  int
	SortBy    string
	SortOrder string
	Status    string
	Search    string
}

func IsReusableInviteSourceCode(code *RedeemCode) bool {
	return code != nil && isInviteIssuerSourceNote(code.Notes)
}

func CanUseInvitationCodeForRegistration(code *RedeemCode) bool {
	if code == nil || code.Type != RedeemTypeInvitation {
		return false
	}
	return code.Status == StatusUnused || IsReusableInviteSourceCode(code)
}

// RedeemService 兑换码服务
type RedeemService struct {
	redeemRepo           RedeemCodeRepository
	userRepo             UserRepository
	subscriptionService  *SubscriptionService
	cache                RedeemCache
	billingCacheService  *BillingCacheService
	entClient            *dbent.Client
	settingService       *SettingService
	authCacheInvalidator APIKeyAuthCacheInvalidator
}

// NewRedeemService 创建兑换码服务实例
func NewRedeemService(
	redeemRepo RedeemCodeRepository,
	userRepo UserRepository,
	subscriptionService *SubscriptionService,
	cache RedeemCache,
	billingCacheService *BillingCacheService,
	entClient *dbent.Client,
	settingService *SettingService,
	authCacheInvalidator APIKeyAuthCacheInvalidator,
) *RedeemService {
	return &RedeemService{
		redeemRepo:           redeemRepo,
		userRepo:             userRepo,
		subscriptionService:  subscriptionService,
		cache:                cache,
		billingCacheService:  billingCacheService,
		entClient:            entClient,
		settingService:       settingService,
		authCacheInvalidator: authCacheInvalidator,
	}
}

// GenerateRandomCode 生成随机兑换码
func (s *RedeemService) GenerateRandomCode() (string, error) {
	// 生成16字节随机数据
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}

	// 转换为十六进制字符串
	code := hex.EncodeToString(bytes)

	// 格式化为 XXXX-XXXX-XXXX-XXXX 格式
	parts := []string{
		strings.ToUpper(code[0:8]),
		strings.ToUpper(code[8:16]),
		strings.ToUpper(code[16:24]),
		strings.ToUpper(code[24:32]),
	}

	return strings.Join(parts, "-"), nil
}

// GenerateCodes 批量生成兑换码
func (s *RedeemService) GenerateCodes(ctx context.Context, req GenerateCodesRequest) ([]RedeemCode, error) {
	if req.Count <= 0 {
		return nil, errors.New("count must be greater than 0")
	}

	// 邀请码类型不需要数值，其他类型需要非零值（支持负数用于退款）
	if req.Type != RedeemTypeInvitation && req.Value == 0 {
		return nil, errors.New("value must not be zero")
	}

	if req.Count > 1000 {
		return nil, errors.New("cannot generate more than 1000 codes at once")
	}

	codeType := req.Type
	if codeType == "" {
		codeType = RedeemTypeBalance
	}

	// 邀请码类型的 value 设为 0
	value := req.Value
	if codeType == RedeemTypeInvitation {
		value = 0
	}

	codes := make([]RedeemCode, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		code, err := s.GenerateRandomCode()
		if err != nil {
			return nil, fmt.Errorf("generate code: %w", err)
		}

		codes = append(codes, RedeemCode{
			Code:   code,
			Type:   codeType,
			Value:  value,
			Status: StatusUnused,
		})
	}

	// 批量插入
	if err := s.redeemRepo.CreateBatch(ctx, codes); err != nil {
		return nil, fmt.Errorf("create batch codes: %w", err)
	}

	return codes, nil
}

// CreateCode creates a redeem code with caller-provided code value.
// It is primarily used by admin integrations that require an external order ID
// to be mapped to a deterministic redeem code.
func (s *RedeemService) CreateCode(ctx context.Context, code *RedeemCode) error {
	if code == nil {
		return errors.New("redeem code is required")
	}
	code.Code = strings.TrimSpace(code.Code)
	if code.Code == "" {
		return errors.New("code is required")
	}
	if code.Type == "" {
		code.Type = RedeemTypeBalance
	}
	if code.Type != RedeemTypeInvitation && code.Value == 0 {
		return errors.New("value must not be zero")
	}
	if code.Status == "" {
		code.Status = StatusUnused
	}

	if err := s.redeemRepo.Create(ctx, code); err != nil {
		return fmt.Errorf("create redeem code: %w", err)
	}
	return nil
}

// GenerateInviteCode 为用户生成邀请码（类型: invitation）。
func (s *RedeemService) GenerateInviteCode(ctx context.Context, issuerUserID int64) (*InviteCodeItem, error) {
	if issuerUserID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_USER_ID", "invalid user id")
	}
	if s.entClient == nil {
		return nil, infraerrors.InternalServer("INTERNAL_ERROR", "ent client not configured")
	}
	if _, err := s.userRepo.GetByID(ctx, issuerUserID); err != nil {
		return nil, fmt.Errorf("get issuer user: %w", err)
	}

	note := buildInviteIssuerNote(issuerUserID)
	existing, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(RedeemTypeInvitation),
			redeemcode.NotesEQ(note),
		).
		Order(dbent.Desc(redeemcode.FieldID)).
		First(ctx)
	if err == nil {
		return &InviteCodeItem{
			Code:      existing.Code,
			Status:    existing.Status,
			UsedBy:    existing.UsedBy,
			UsedAt:    existing.UsedAt,
			CreatedAt: existing.CreatedAt,
		}, nil
	}
	if err != nil && !dbent.IsNotFound(err) {
		return nil, fmt.Errorf("query invite code: %w", err)
	}

	codeValue, err := GenerateRedeemCode()
	if err != nil {
		return nil, fmt.Errorf("generate invite code: %w", err)
	}

	code := &RedeemCode{
		Code:   codeValue,
		Type:   RedeemTypeInvitation,
		Value:  0,
		Status: StatusUnused,
		Notes:  note,
	}
	if err := s.redeemRepo.Create(ctx, code); err != nil {
		return nil, fmt.Errorf("create invite code: %w", err)
	}

	return &InviteCodeItem{
		Code:      code.Code,
		Status:    code.Status,
		UsedBy:    code.UsedBy,
		UsedAt:    code.UsedAt,
		CreatedAt: code.CreatedAt,
	}, nil
}

// GetInviteOverview 返回用户邀请返现概览。
func (s *RedeemService) GetInviteOverview(ctx context.Context, issuerUserID int64, query InviteOverviewQuery) (*InviteOverview, error) {
	if s.entClient == nil {
		return nil, infraerrors.InternalServer("INTERNAL_ERROR", "ent client not configured")
	}
	query = normalizeInviteOverviewQuery(query)

	note := buildInviteIssuerNote(issuerUserID)
	usageNote := buildInviteUsageNote(issuerUserID)
	codes, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(RedeemTypeInvitation),
			redeemcode.NotesEQ(note),
		).
		Order(dbent.Desc(redeemcode.FieldID)).
		Limit(query.CodesLimit).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("list invite codes: %w", err)
	}

	outCodes := make([]InviteCodeItem, 0, len(codes))
	for i := range codes {
		outCodes = append(outCodes, InviteCodeItem{
			Code:      codes[i].Code,
			Status:    codes[i].Status,
			UsedBy:    codes[i].UsedBy,
			UsedAt:    codes[i].UsedAt,
			CreatedAt: codes[i].CreatedAt,
		})
	}

	oldInvitedUsers, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(RedeemTypeInvitation),
			redeemcode.NotesEQ(note),
			redeemcode.StatusEQ(StatusUsed),
			redeemcode.UsedByNotNil(),
		).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("count invited users: %w", err)
	}

	usageInvitedUsers, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(RedeemTypeInvitation),
			redeemcode.NotesEQ(usageNote),
			redeemcode.StatusEQ(StatusUsed),
			redeemcode.UsedByNotNil(),
		).
		Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("count invite usage records: %w", err)
	}

	totalCashback, err := sumRedeemValue(ctx, s.entClient,
		redeemcode.TypeEQ(AdjustmentTypeInviteCashback),
		redeemcode.UsedByEQ(issuerUserID),
		redeemcode.ValueGT(0),
	)
	if err != nil {
		return nil, fmt.Errorf("sum invite cashback: %w", err)
	}

	records, err := s.listInviteRecords(ctx, note, usageNote, query)
	if err != nil {
		return nil, fmt.Errorf("list invite records: %w", err)
	}

	return &InviteOverview{
		CashbackRate:  s.getInviteCashbackRate(ctx),
		InvitedUsers:  int64(oldInvitedUsers + usageInvitedUsers),
		TotalCashback: totalCashback,
		Codes:         outCodes,
		Records:       records,
	}, nil
}

func normalizeInviteOverviewQuery(query InviteOverviewQuery) InviteOverviewQuery {
	if query.CodesLimit <= 0 {
		query.CodesLimit = defaultInviteOverviewLimit
	}
	if query.CodesLimit > maxInviteOverviewLimit {
		query.CodesLimit = maxInviteOverviewLimit
	}
	if query.RecordsPage <= 0 {
		query.RecordsPage = 1
	}
	if query.RecordsPageSize <= 0 {
		query.RecordsPageSize = defaultInviteRecordsPageSize
	}
	if query.RecordsPageSize > maxInviteRecordsPageSize {
		query.RecordsPageSize = maxInviteRecordsPageSize
	}
	switch query.RecordsSortBy {
	case inviteRecordSortRegisteredAt, inviteRecordSortLastCashback, inviteRecordSortTotalCashback:
	default:
		query.RecordsSortBy = inviteRecordSortRegisteredAt
	}
	switch query.RecordsSortOrder {
	case inviteSortOrderAsc, inviteSortOrderDesc:
	default:
		query.RecordsSortOrder = inviteSortOrderDesc
	}
	switch query.RecordsStatus {
	case inviteStatusAll, inviteStatusRecharged, inviteStatusNotRecharged:
	default:
		query.RecordsStatus = inviteStatusAll
	}
	return query
}

func (s *RedeemService) listInviteRecords(ctx context.Context, sourceNote, usageNote string, query InviteOverviewQuery) (InviteRecordList, error) {
	inviteEntries, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(RedeemTypeInvitation),
			redeemcode.UsedByNotNil(),
			redeemcode.Or(
				redeemcode.NotesEQ(sourceNote),
				redeemcode.NotesEQ(usageNote),
			),
		).
		Order(
			dbent.Desc(redeemcode.FieldUsedAt),
			dbent.Desc(redeemcode.FieldID),
		).
		All(ctx)
	if err != nil {
		return InviteRecordList{}, err
	}
	if len(inviteEntries) == 0 {
		return InviteRecordList{
			Items:     []InviteRecord{},
			Total:     0,
			Page:      query.RecordsPage,
			PageSize:  query.RecordsPageSize,
			Pages:     1,
			SortBy:    query.RecordsSortBy,
			SortOrder: query.RecordsSortOrder,
			Status:    query.RecordsStatus,
		}, nil
	}

	userIDs := make([]int64, 0, len(inviteEntries))
	recordByUserID := make(map[int64]*InviteRecord, len(inviteEntries))
	for i := range inviteEntries {
		if inviteEntries[i].UsedBy == nil || *inviteEntries[i].UsedBy <= 0 {
			continue
		}
		invitedUserID := *inviteEntries[i].UsedBy
		if _, exists := recordByUserID[invitedUserID]; exists {
			continue
		}
		record := &InviteRecord{
			InvitedUserID: invitedUserID,
			InviteUsedAt:  inviteEntries[i].UsedAt,
		}
		if inviteEntries[i].UsedAt != nil {
			record.RegisteredAt = *inviteEntries[i].UsedAt
		}
		recordByUserID[invitedUserID] = record
		userIDs = append(userIDs, invitedUserID)
	}
	if len(userIDs) == 0 {
		return InviteRecordList{
			Items:     []InviteRecord{},
			Total:     0,
			Page:      query.RecordsPage,
			PageSize:  query.RecordsPageSize,
			Pages:     1,
			SortBy:    query.RecordsSortBy,
			SortOrder: query.RecordsSortOrder,
			Status:    query.RecordsStatus,
		}, nil
	}

	users, err := s.entClient.User.Query().
		Where(dbuser.IDIn(userIDs...)).
		All(ctx)
	if err != nil {
		return InviteRecordList{}, err
	}
	for i := range users {
		record := recordByUserID[users[i].ID]
		if record == nil {
			continue
		}
		record.InvitedUserEmailMasked = MaskEmail(users[i].Email)
		record.RegisteredAt = users[i].CreatedAt
	}

	cashbackRecords, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(AdjustmentTypeInviteCashback),
			redeemcode.UsedByEQ(issuerUserID),
			redeemcode.NotesHasPrefix(inviteCashbackNotePrefix),
		).
		All(ctx)
	if err != nil {
		return InviteRecordList{}, err
	}
	for i := range cashbackRecords {
		note := ""
		if cashbackRecords[i].Notes != nil {
			note = *cashbackRecords[i].Notes
		}
		invitedUserID, ok := parseInviteCashbackInvitedUserID(note)
		if !ok {
			continue
		}
		record := recordByUserID[invitedUserID]
		if record == nil {
			continue
		}
		record.TotalCashback += cashbackRecords[i].Value
		record.CashbackCount++
		if cashbackRecords[i].UsedAt != nil {
			if record.LastCashbackAt == nil || cashbackRecords[i].UsedAt.After(*record.LastCashbackAt) {
				ts := *cashbackRecords[i].UsedAt
				record.LastCashbackAt = &ts
			}
		}
	}

	records := make([]InviteRecord, 0, len(userIDs))
	for _, invitedUserID := range userIDs {
		record := recordByUserID[invitedUserID]
		if record == nil {
			continue
		}
		if record.InvitedUserEmailMasked == "" {
			record.InvitedUserEmailMasked = "Unknown"
		}
		records = append(records, *record)
	}
	records = filterInviteRecords(records, query)
	sortInviteRecords(records, query.RecordsSortBy, query.RecordsSortOrder)
	total := len(records)
	page, pageSize, pages := paginateInviteList(query.RecordsPage, query.RecordsPageSize, total)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	items := make([]InviteRecord, 0, end-start)
	if start < end {
		items = append(items, records[start:end]...)
	}

	return InviteRecordList{
		Items:     items,
		Total:     int64(total),
		Page:      page,
		PageSize:  pageSize,
		Pages:     pages,
		SortBy:    query.RecordsSortBy,
		SortOrder: query.RecordsSortOrder,
		Status:    query.RecordsStatus,
	}, nil
}

func filterInviteRecords(records []InviteRecord, query InviteOverviewQuery) []InviteRecord {
	if len(records) == 0 {
		return records
	}

	filtered := make([]InviteRecord, 0, len(records))
	for _, record := range records {
		if query.RecordsStatus == inviteStatusRecharged && record.TotalCashback <= 0 {
			continue
		}
		if query.RecordsStatus == inviteStatusNotRecharged && record.TotalCashback > 0 {
			continue
		}
		if query.RecordsDateFrom != nil && record.RegisteredAt.Before(*query.RecordsDateFrom) {
			continue
		}
		if query.RecordsDateTo != nil && !record.RegisteredAt.Before(*query.RecordsDateTo) {
			continue
		}
		filtered = append(filtered, record)
	}
	return filtered
}

func sortInviteRecords(records []InviteRecord, sortBy, sortOrder string) {
	desc := sortOrder != inviteSortOrderAsc
	sort.Slice(records, func(i, j int) bool {
		switch sortBy {
		case inviteRecordSortLastCashback:
			return compareOptionalTimes(records[i].LastCashbackAt, records[j].LastCashbackAt, desc)
		case inviteRecordSortTotalCashback:
			if records[i].TotalCashback == records[j].TotalCashback {
				return compareTimes(records[i].RegisteredAt, records[j].RegisteredAt, true)
			}
			if desc {
				return records[i].TotalCashback > records[j].TotalCashback
			}
			return records[i].TotalCashback < records[j].TotalCashback
		default:
			return compareTimes(records[i].RegisteredAt, records[j].RegisteredAt, desc)
		}
	})
}

func paginateInviteList(page, pageSize, total int) (int, int, int) {
	if pageSize <= 0 {
		pageSize = defaultInviteRecordsPageSize
	}
	pages := 1
	if total > 0 {
		pages = (total + pageSize - 1) / pageSize
	}
	if page <= 0 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	return page, pageSize, pages
}

func compareTimes(a, b time.Time, desc bool) bool {
	if a.Equal(b) {
		return false
	}
	if desc {
		return a.After(b)
	}
	return a.Before(b)
}

func compareOptionalTimes(a, b *time.Time, desc bool) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil:
		return false
	case b == nil:
		return true
	default:
		return compareTimes(*a, *b, desc)
	}
}

func normalizeInviteLeaderboardQuery(query InviteLeaderboardQuery) InviteLeaderboardQuery {
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 {
		query.PageSize = defaultInviteRankingPageSize
	}
	if query.PageSize > maxInviteRankingPageSize {
		query.PageSize = maxInviteRankingPageSize
	}
	switch query.SortBy {
	case inviteRankingSortInvitedUsers, inviteRankingSortTotalCashback, inviteRankingSortLastInviteAt, inviteRankingSortLastCashback:
	default:
		query.SortBy = inviteRankingSortTotalCashback
	}
	switch query.SortOrder {
	case inviteSortOrderAsc, inviteSortOrderDesc:
	default:
		query.SortOrder = inviteSortOrderDesc
	}
	switch query.Status {
	case inviteStatusAll, inviteStatusRecharged, inviteStatusNotRecharged:
	default:
		query.Status = inviteStatusAll
	}
	query.Search = strings.TrimSpace(query.Search)
	return query
}

func (s *RedeemService) GetInviteLeaderboard(ctx context.Context, query InviteLeaderboardQuery) (*InviteLeaderboardPage, error) {
	if s.entClient == nil {
		return nil, infraerrors.InternalServer("INTERNAL_ERROR", "ent client not configured")
	}

	query = normalizeInviteLeaderboardQuery(query)

	inviteEntries, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(RedeemTypeInvitation),
			redeemcode.UsedByNotNil(),
			redeemcode.Or(
				redeemcode.NotesHasPrefix(inviteCodeIssuerNotePrefix),
				redeemcode.NotesHasPrefix(inviteUsageNotePrefix),
			),
		).
		Order(
			dbent.Desc(redeemcode.FieldUsedAt),
			dbent.Desc(redeemcode.FieldID),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(inviteEntries) == 0 {
		return &InviteLeaderboardPage{
			Items:     []InviteLeaderboardItem{},
			Total:     0,
			Page:      query.Page,
			PageSize:  query.PageSize,
			Pages:     1,
			SortBy:    query.SortBy,
			SortOrder: query.SortOrder,
			Status:    query.Status,
			Search:    query.Search,
		}, nil
	}

	type aggregate struct {
		item          InviteLeaderboardItem
		invitedUserIDs map[int64]struct{}
	}

	aggregates := make(map[int64]*aggregate)
	inviterIDs := make([]int64, 0)
	for i := range inviteEntries {
		if inviteEntries[i].UsedBy == nil || *inviteEntries[i].UsedBy <= 0 {
			continue
		}
		note := ""
		if inviteEntries[i].Notes != nil {
			note = *inviteEntries[i].Notes
		}
		inviterID, ok := parseInviteIssuerUserID(note)
		if !ok || inviterID <= 0 {
			continue
		}
		agg, exists := aggregates[inviterID]
		if !exists {
			agg = &aggregate{
				item: InviteLeaderboardItem{
					InviterUserID: inviterID,
				},
				invitedUserIDs: make(map[int64]struct{}),
			}
			aggregates[inviterID] = agg
			inviterIDs = append(inviterIDs, inviterID)
		}
		invitedUserID := *inviteEntries[i].UsedBy
		agg.invitedUserIDs[invitedUserID] = struct{}{}
		if inviteEntries[i].UsedAt != nil {
			if agg.item.LastInviteAt == nil || inviteEntries[i].UsedAt.After(*agg.item.LastInviteAt) {
				ts := *inviteEntries[i].UsedAt
				agg.item.LastInviteAt = &ts
			}
		}
	}

	if len(inviterIDs) == 0 {
		return &InviteLeaderboardPage{
			Items:     []InviteLeaderboardItem{},
			Total:     0,
			Page:      query.Page,
			PageSize:  query.PageSize,
			Pages:     1,
			SortBy:    query.SortBy,
			SortOrder: query.SortOrder,
			Status:    query.Status,
			Search:    query.Search,
		}, nil
	}

	users, err := s.entClient.User.Query().
		Where(dbuser.IDIn(inviterIDs...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if agg := aggregates[users[i].ID]; agg != nil {
			agg.item.InviterEmail = users[i].Email
			agg.item.InviterUsername = users[i].Username
		}
	}

	cashbackRecords, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(AdjustmentTypeInviteCashback),
			redeemcode.UsedByIn(inviterIDs...),
			redeemcode.NotesHasPrefix(inviteCashbackNotePrefix),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for i := range cashbackRecords {
		if cashbackRecords[i].UsedBy == nil {
			continue
		}
		agg := aggregates[*cashbackRecords[i].UsedBy]
		if agg == nil {
			continue
		}
		agg.item.TotalCashback += cashbackRecords[i].Value
		agg.item.CashbackCount++
		if cashbackRecords[i].UsedAt != nil {
			if agg.item.LastCashbackAt == nil || cashbackRecords[i].UsedAt.After(*agg.item.LastCashbackAt) {
				ts := *cashbackRecords[i].UsedAt
				agg.item.LastCashbackAt = &ts
			}
		}
	}

	items := make([]InviteLeaderboardItem, 0, len(aggregates))
	for _, inviterID := range inviterIDs {
		agg := aggregates[inviterID]
		if agg == nil {
			continue
		}
		agg.item.InvitedUsers = int64(len(agg.invitedUserIDs))
		items = append(items, agg.item)
	}

	items = filterInviteLeaderboard(items, query)
	sortInviteLeaderboard(items, query.SortBy, query.SortOrder)
	total := len(items)
	page, pageSize, pages := paginateInviteList(query.Page, query.PageSize, total)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	pagedItems := make([]InviteLeaderboardItem, 0, end-start)
	if start < end {
		pagedItems = append(pagedItems, items[start:end]...)
	}

	return &InviteLeaderboardPage{
		Items:     pagedItems,
		Total:     int64(total),
		Page:      page,
		PageSize:  pageSize,
		Pages:     pages,
		SortBy:    query.SortBy,
		SortOrder: query.SortOrder,
		Status:    query.Status,
		Search:    query.Search,
	}, nil
}

func filterInviteLeaderboard(items []InviteLeaderboardItem, query InviteLeaderboardQuery) []InviteLeaderboardItem {
	if len(items) == 0 {
		return items
	}
	filtered := make([]InviteLeaderboardItem, 0, len(items))
	search := strings.ToLower(strings.TrimSpace(query.Search))
	for _, item := range items {
		if query.Status == inviteStatusRecharged && item.TotalCashback <= 0 {
			continue
		}
		if query.Status == inviteStatusNotRecharged && item.TotalCashback > 0 {
			continue
		}
		if search != "" {
			idText := strconv.FormatInt(item.InviterUserID, 10)
			if !strings.Contains(strings.ToLower(item.InviterEmail), search) &&
				!strings.Contains(strings.ToLower(item.InviterUsername), search) &&
				!strings.Contains(idText, search) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func sortInviteLeaderboard(items []InviteLeaderboardItem, sortBy, sortOrder string) {
	desc := sortOrder != inviteSortOrderAsc
	sort.Slice(items, func(i, j int) bool {
		switch sortBy {
		case inviteRankingSortInvitedUsers:
			if items[i].InvitedUsers == items[j].InvitedUsers {
				return compareOptionalTimes(items[i].LastInviteAt, items[j].LastInviteAt, true)
			}
			if desc {
				return items[i].InvitedUsers > items[j].InvitedUsers
			}
			return items[i].InvitedUsers < items[j].InvitedUsers
		case inviteRankingSortLastInviteAt:
			return compareOptionalTimes(items[i].LastInviteAt, items[j].LastInviteAt, desc)
		case inviteRankingSortLastCashback:
			return compareOptionalTimes(items[i].LastCashbackAt, items[j].LastCashbackAt, desc)
		default:
			if items[i].TotalCashback == items[j].TotalCashback {
				return compareOptionalTimes(items[i].LastCashbackAt, items[j].LastCashbackAt, true)
			}
			if desc {
				return items[i].TotalCashback > items[j].TotalCashback
			}
			return items[i].TotalCashback < items[j].TotalCashback
		}
	})
}

// checkRedeemRateLimit 检查用户兑换错误次数是否超限
func (s *RedeemService) checkRedeemRateLimit(ctx context.Context, userID int64) error {
	if s.cache == nil {
		return nil
	}

	count, err := s.cache.GetRedeemAttemptCount(ctx, userID)
	if err != nil {
		// Redis 出错时不阻止用户操作
		return nil
	}

	if count >= redeemMaxErrorsPerHour {
		return ErrRedeemRateLimited
	}

	return nil
}

// incrementRedeemErrorCount 增加用户兑换错误计数
func (s *RedeemService) incrementRedeemErrorCount(ctx context.Context, userID int64) {
	if s.cache == nil {
		return
	}

	_ = s.cache.IncrementRedeemAttemptCount(ctx, userID)
}

// acquireRedeemLock 尝试获取兑换码的分布式锁
// 返回 true 表示获取成功，false 表示锁已被占用
func (s *RedeemService) acquireRedeemLock(ctx context.Context, code string) bool {
	if s.cache == nil {
		return true // 无 Redis 时降级为不加锁
	}

	ok, err := s.cache.AcquireRedeemLock(ctx, code, redeemLockDuration)
	if err != nil {
		// Redis 出错时不阻止操作，依赖数据库层面的状态检查
		return true
	}
	return ok
}

// releaseRedeemLock 释放兑换码的分布式锁
func (s *RedeemService) releaseRedeemLock(ctx context.Context, code string) {
	if s.cache == nil {
		return
	}

	_ = s.cache.ReleaseRedeemLock(ctx, code)
}

// Redeem 使用兑换码
func (s *RedeemService) Redeem(ctx context.Context, userID int64, code string) (*RedeemCode, error) {
	// 检查限流
	if err := s.checkRedeemRateLimit(ctx, userID); err != nil {
		return nil, err
	}

	// 获取分布式锁，防止同一兑换码并发使用
	if !s.acquireRedeemLock(ctx, code) {
		return nil, ErrRedeemCodeLocked
	}
	defer s.releaseRedeemLock(ctx, code)

	// 查找兑换码
	redeemCode, err := s.redeemRepo.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, ErrRedeemCodeNotFound) {
			s.incrementRedeemErrorCount(ctx, userID)
			return nil, ErrRedeemCodeNotFound
		}
		return nil, fmt.Errorf("get redeem code: %w", err)
	}

	// 检查兑换码状态
	if !redeemCode.CanUse() {
		s.incrementRedeemErrorCount(ctx, userID)
		return nil, ErrRedeemCodeUsed
	}

	// 验证兑换码类型的前置条件
	if redeemCode.Type == RedeemTypeSubscription && redeemCode.GroupID == nil {
		return nil, infraerrors.BadRequest("REDEEM_CODE_INVALID", "invalid subscription redeem code: missing group_id")
	}

	// 获取用户信息
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	// 使用数据库事务保证兑换码标记与权益发放的原子性
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 将事务放入 context，使 repository 方法能够使用同一事务
	txCtx := dbent.NewTxContext(ctx, tx)

	// 【关键】先标记兑换码为已使用，确保并发安全
	// 利用数据库乐观锁（WHERE status = 'unused'）保证原子性
	if err := s.redeemRepo.Use(txCtx, redeemCode.ID, userID); err != nil {
		if errors.Is(err, ErrRedeemCodeNotFound) || errors.Is(err, ErrRedeemCodeUsed) {
			return nil, ErrRedeemCodeUsed
		}
		return nil, fmt.Errorf("mark code as used: %w", err)
	}

	// 执行兑换逻辑（兑换码已被锁定，此时可安全操作）
	var inviteCashbackReceiverID *int64
	switch redeemCode.Type {
	case RedeemTypeBalance:
		amount := redeemCode.Value
		// 负数为退款扣减，余额最低为 0
		if amount < 0 && user.Balance+amount < 0 {
			amount = -user.Balance
		}
		if err := s.userRepo.UpdateBalance(txCtx, userID, amount); err != nil {
			return nil, fmt.Errorf("update user balance: %w", err)
		}
		inviteCashbackReceiverID, err = s.applyInviteCashback(txCtx, userID, amount)
		if err != nil {
			return nil, fmt.Errorf("apply invite cashback: %w", err)
		}

	case RedeemTypeConcurrency:
		delta := int(redeemCode.Value)
		// 负数为退款扣减，并发数最低为 0
		if delta < 0 && user.Concurrency+delta < 0 {
			delta = -user.Concurrency
		}
		if err := s.userRepo.UpdateConcurrency(txCtx, userID, delta); err != nil {
			return nil, fmt.Errorf("update user concurrency: %w", err)
		}

	case RedeemTypeSubscription:
		validityDays := redeemCode.ValidityDays
		if validityDays < 0 {
			// 负数天数：缩短订阅，减到 0 则取消订阅
			if err := s.reduceOrCancelSubscription(txCtx, userID, *redeemCode.GroupID, -validityDays, redeemCode.Code); err != nil {
				return nil, fmt.Errorf("reduce or cancel subscription: %w", err)
			}
		} else {
			if validityDays == 0 {
				validityDays = 30
			}
			_, _, err := s.subscriptionService.AssignOrExtendSubscription(txCtx, &AssignSubscriptionInput{
				UserID:       userID,
				GroupID:      *redeemCode.GroupID,
				ValidityDays: validityDays,
				AssignedBy:   0, // 系统分配
				Notes:        fmt.Sprintf("通过兑换码 %s 兑换", redeemCode.Code),
			})
			if err != nil {
				return nil, fmt.Errorf("assign or extend subscription: %w", err)
			}
		}

	default:
		return nil, fmt.Errorf("unsupported redeem type: %s", redeemCode.Type)
	}

	// 提交事务
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	// 事务提交成功后失效缓存
	s.invalidateRedeemCaches(ctx, userID, redeemCode)
	if inviteCashbackReceiverID != nil {
		s.invalidateUserBalanceCache(ctx, *inviteCashbackReceiverID)
	}

	// 重新获取更新后的兑换码
	redeemCode, err = s.redeemRepo.GetByID(ctx, redeemCode.ID)
	if err != nil {
		return nil, fmt.Errorf("get updated redeem code: %w", err)
	}

	return redeemCode, nil
}

// invalidateRedeemCaches 失效兑换相关的缓存
func (s *RedeemService) invalidateRedeemCaches(ctx context.Context, userID int64, redeemCode *RedeemCode) {
	switch redeemCode.Type {
	case RedeemTypeBalance:
		s.invalidateUserBalanceCache(ctx, userID)
	case RedeemTypeConcurrency:
		if s.authCacheInvalidator != nil {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
		}
		if s.billingCacheService == nil {
			return
		}
	case RedeemTypeSubscription:
		if s.authCacheInvalidator != nil {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
		}
		if s.billingCacheService == nil {
			return
		}
		if redeemCode.GroupID != nil {
			groupID := *redeemCode.GroupID
			go func() {
				cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = s.billingCacheService.InvalidateSubscription(cacheCtx, userID, groupID)
			}()
		}
	}
}

func (s *RedeemService) invalidateUserBalanceCache(ctx context.Context, userID int64) {
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
	}
	if s.billingCacheService == nil {
		return
	}
	go func() {
		cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.billingCacheService.InvalidateUserBalance(cacheCtx, userID)
	}()
}

func (s *RedeemService) applyInviteCashback(ctx context.Context, invitedUserID int64, rechargeAmount float64) (*int64, error) {
	if rechargeAmount <= 0 {
		return nil, nil
	}

	inviterID, err := s.findInviterByInvitedUser(ctx, invitedUserID)
	if err != nil {
		return nil, err
	}
	if inviterID == nil {
		return nil, nil
	}

	cashbackRate := s.getInviteCashbackRate(ctx) / 100
	cashbackAmount := math.Round(rechargeAmount*cashbackRate*inviteCashbackAmountDecimal) / inviteCashbackAmountDecimal
	if cashbackAmount <= 0 {
		return nil, nil
	}

	if err := s.userRepo.UpdateBalance(ctx, *inviterID, cashbackAmount); err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("update inviter balance: %w", err)
	}

	codeValue, err := GenerateRedeemCode()
	if err != nil {
		return nil, fmt.Errorf("generate cashback record code: %w", err)
	}
	now := time.Now()
	record := &RedeemCode{
		Code:   codeValue,
		Type:   AdjustmentTypeInviteCashback,
		Value:  cashbackAmount,
		Status: StatusUsed,
		UsedBy: inviterID,
		UsedAt: &now,
		Notes:  buildInviteCashbackNote(invitedUserID),
	}
	if err := s.redeemRepo.Create(ctx, record); err != nil {
		return nil, fmt.Errorf("create cashback record: %w", err)
	}

	return inviterID, nil
}

func (s *RedeemService) getInviteCashbackRate(ctx context.Context) float64 {
	if s.settingService == nil {
		return DefaultInviteCashbackRate
	}
	return s.settingService.GetInviteCashbackRate(ctx)
}

func (s *RedeemService) findInviterByInvitedUser(ctx context.Context, invitedUserID int64) (*int64, error) {
	if s.entClient == nil || invitedUserID <= 0 {
		return nil, nil
	}

	invitation, err := s.entClient.RedeemCode.Query().
		Where(
			redeemcode.TypeEQ(RedeemTypeInvitation),
			redeemcode.UsedByEQ(invitedUserID),
			redeemcode.Or(
				redeemcode.NotesHasPrefix(inviteUsageNotePrefix),
				redeemcode.NotesHasPrefix(inviteCodeIssuerNotePrefix),
			),
		).
		Order(
			dbent.Desc(redeemcode.FieldUsedAt),
			dbent.Desc(redeemcode.FieldID),
		).
		First(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("query invitation record: %w", err)
	}

	if invitation.Notes == nil {
		return nil, nil
	}
	inviterID, ok := parseInviteIssuerUserID(*invitation.Notes)
	if !ok || inviterID == invitedUserID {
		return nil, nil
	}
	return &inviterID, nil
}

func buildInviteIssuerNote(userID int64) string {
	return inviteCodeIssuerNotePrefix + strconv.FormatInt(userID, 10)
}

func buildInviteUsageNote(userID int64) string {
	return inviteUsageNotePrefix + strconv.FormatInt(userID, 10)
}

func isInviteIssuerSourceNote(note string) bool {
	return strings.HasPrefix(note, inviteCodeIssuerNotePrefix)
}

func parseInviteIssuerUserID(note string) (int64, bool) {
	for _, prefix := range []string{inviteCodeIssuerNotePrefix, inviteUsageNotePrefix} {
		if !strings.HasPrefix(note, prefix) {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(note, prefix))
		if raw == "" {
			return 0, false
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return 0, false
		}
		return id, true
	}
	return 0, false
}

func buildInviteCashbackNote(invitedUserID int64) string {
	return inviteCashbackNotePrefix + strconv.FormatInt(invitedUserID, 10)
}

func parseInviteCashbackInvitedUserID(note string) (int64, bool) {
	if !strings.HasPrefix(note, inviteCashbackNotePrefix) {
		return 0, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(note, inviteCashbackNotePrefix))
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func sumRedeemValue(ctx context.Context, client *dbent.Client, preds ...predicate.RedeemCode) (float64, error) {
	var result []struct {
		Sum float64 `json:"sum"`
	}
	err := client.RedeemCode.Query().
		Where(preds...).
		Aggregate(dbent.As(dbent.Sum(redeemcode.FieldValue), "sum")).
		Scan(ctx, &result)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, nil
	}
	return result[0].Sum, nil
}

// GetByID 根据ID获取兑换码
func (s *RedeemService) GetByID(ctx context.Context, id int64) (*RedeemCode, error) {
	code, err := s.redeemRepo.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get redeem code: %w", err)
	}
	return code, nil
}

// GetByCode 根据Code获取兑换码
func (s *RedeemService) GetByCode(ctx context.Context, code string) (*RedeemCode, error) {
	redeemCode, err := s.redeemRepo.GetByCode(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("get redeem code: %w", err)
	}
	return redeemCode, nil
}

// List 获取兑换码列表（管理员功能）
func (s *RedeemService) List(ctx context.Context, params pagination.PaginationParams) ([]RedeemCode, *pagination.PaginationResult, error) {
	codes, pagination, err := s.redeemRepo.List(ctx, params)
	if err != nil {
		return nil, nil, fmt.Errorf("list redeem codes: %w", err)
	}
	return codes, pagination, nil
}

// Delete 删除兑换码（管理员功能）
func (s *RedeemService) Delete(ctx context.Context, id int64) error {
	// 检查兑换码是否存在
	code, err := s.redeemRepo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("get redeem code: %w", err)
	}

	// 不允许删除已使用的兑换码
	if code.IsUsed() {
		return infraerrors.Conflict("REDEEM_CODE_DELETE_USED", "cannot delete used redeem code")
	}

	if err := s.redeemRepo.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete redeem code: %w", err)
	}

	return nil
}

// GetStats 获取兑换码统计信息
func (s *RedeemService) GetStats(ctx context.Context) (map[string]any, error) {
	// TODO: 实现统计逻辑
	// 统计未使用、已使用的兑换码数量
	// 统计总面值等

	stats := map[string]any{
		"total_codes":  0,
		"unused_codes": 0,
		"used_codes":   0,
		"total_value":  0.0,
	}

	return stats, nil
}

// GetUserHistory 获取用户的兑换历史
func (s *RedeemService) GetUserHistory(ctx context.Context, userID int64, limit int) ([]RedeemCode, error) {
	codes, err := s.redeemRepo.ListByUser(ctx, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("get user redeem history: %w", err)
	}
	return codes, nil
}

// reduceOrCancelSubscription 缩短订阅天数，剩余天数 <= 0 时取消订阅
func (s *RedeemService) reduceOrCancelSubscription(ctx context.Context, userID, groupID int64, reduceDays int, code string) error {
	sub, err := s.subscriptionService.userSubRepo.GetByUserIDAndGroupID(ctx, userID, groupID)
	if err != nil {
		return ErrSubscriptionNotFound
	}

	now := time.Now()
	remaining := int(sub.ExpiresAt.Sub(now).Hours() / 24)
	if remaining < 0 {
		remaining = 0
	}

	notes := fmt.Sprintf("通过兑换码 %s 退款扣减 %d 天", code, reduceDays)

	if remaining <= reduceDays {
		// 剩余天数不足，直接取消订阅
		if err := s.subscriptionService.userSubRepo.UpdateStatus(ctx, sub.ID, SubscriptionStatusExpired); err != nil {
			return fmt.Errorf("cancel subscription: %w", err)
		}
		// 设置过期时间为当前时间
		if err := s.subscriptionService.userSubRepo.ExtendExpiry(ctx, sub.ID, now); err != nil {
			return fmt.Errorf("set subscription expiry: %w", err)
		}
	} else {
		// 缩短天数
		newExpiresAt := sub.ExpiresAt.AddDate(0, 0, -reduceDays)
		if err := s.subscriptionService.userSubRepo.ExtendExpiry(ctx, sub.ID, newExpiresAt); err != nil {
			return fmt.Errorf("reduce subscription: %w", err)
		}
	}

	// 追加备注
	newNotes := sub.Notes
	if newNotes != "" {
		newNotes += "\n"
	}
	newNotes += notes
	if err := s.subscriptionService.userSubRepo.UpdateNotes(ctx, sub.ID, newNotes); err != nil {
		return fmt.Errorf("update subscription notes: %w", err)
	}

	// 失效缓存
	s.subscriptionService.InvalidateSubCache(userID, groupID)

	return nil
}
