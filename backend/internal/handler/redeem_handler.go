package handler

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RedeemHandler handles redeem code-related requests
type RedeemHandler struct {
	redeemService *service.RedeemService
}

// NewRedeemHandler creates a new RedeemHandler
func NewRedeemHandler(redeemService *service.RedeemService) *RedeemHandler {
	return &RedeemHandler{
		redeemService: redeemService,
	}
}

// RedeemRequest represents the redeem code request payload
type RedeemRequest struct {
	Code string `json:"code" binding:"required"`
}

// RedeemResponse represents the redeem response
type RedeemResponse struct {
	Message        string   `json:"message"`
	Type           string   `json:"type"`
	Value          float64  `json:"value"`
	NewBalance     *float64 `json:"new_balance,omitempty"`
	NewConcurrency *int     `json:"new_concurrency,omitempty"`
}

// Redeem handles redeeming a code
// POST /api/v1/redeem
func (h *RedeemHandler) Redeem(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	var req RedeemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.redeemService.Redeem(c.Request.Context(), subject.UserID, req.Code)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.RedeemCodeFromService(result))
}

// GetHistory returns the user's redemption history
// GET /api/v1/redeem/history
func (h *RedeemHandler) GetHistory(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	// Default limit is 25
	limit := 25

	codes, err := h.redeemService.GetUserHistory(c.Request.Context(), subject.UserID, limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.RedeemCode, 0, len(codes))
	for i := range codes {
		out = append(out, *dto.RedeemCodeFromService(&codes[i]))
	}
	response.Success(c, out)
}

// GenerateInviteCode 生成当前用户的邀请码
// POST /api/v1/redeem/invite-code
func (h *RedeemHandler) GenerateInviteCode(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	code, err := h.redeemService.GenerateInviteCode(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, code)
}

// GetInviteOverview 获取当前用户邀请返现概览
// GET /api/v1/redeem/invite-overview
func (h *RedeemHandler) GetInviteOverview(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	query := service.InviteOverviewQuery{}
	limit := 10
	if rawLimit := c.Query("limit"); rawLimit != "" {
		parsedLimit, err := strconv.Atoi(rawLimit)
		if err != nil || parsedLimit <= 0 {
			response.BadRequest(c, "Invalid limit")
			return
		}
		limit = parsedLimit
	}
	query.CodesLimit = limit
	query.RecordsPage = parsePositiveIntQuery(c.Query("records_page"), 1)
	query.RecordsPageSize = parsePositiveIntQuery(c.Query("records_page_size"), 10)
	query.RecordsSortBy = c.DefaultQuery("sort_by", "registered_at")
	query.RecordsSortOrder = c.DefaultQuery("sort_order", "desc")
	query.RecordsStatus = c.Query("status")

	if raw := c.Query("date_from"); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			response.BadRequest(c, "Invalid date_from")
			return
		}
		query.RecordsDateFrom = &parsed
	}
	if raw := c.Query("date_to"); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			response.BadRequest(c, "Invalid date_to")
			return
		}
		endExclusive := parsed.Add(24 * time.Hour)
		query.RecordsDateTo = &endExclusive
	}

	overview, err := h.redeemService.GetInviteOverview(c.Request.Context(), subject.UserID, query)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, overview)
}

func parsePositiveIntQuery(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
