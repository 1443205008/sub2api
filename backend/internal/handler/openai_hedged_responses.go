package handler

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type openAIHedgedResponsesInput struct {
	APIKey         *service.APIKey
	Subject        middleware2.AuthSubject
	Subscription   *service.UserSubscription
	Body           []byte
	ForwardBody    []byte
	RequestModel   string
	SessionHash    string
	RequireCompact bool
	ChannelMapping service.ChannelMappingResult
	RoutingStart    time.Time
	RequestLog      *zap.Logger
}

type openAIHedgedCandidate struct {
	ID      int
	Account *service.Account
	Release func()
}

type openAIHedgedCandidateResult struct {
	Candidate *openAIHedgedCandidate
	Result    *service.OpenAIForwardResult
	Err       error
}

type openAIHedgedCandidateRun struct {
	Candidate *openAIHedgedCandidate
	Context   context.Context
	Cancel    context.CancelFunc
	Shadow    *gin.Context
}

func (h *OpenAIGatewayHandler) tryForwardResponsesHedged(c *gin.Context, in openAIHedgedResponsesInput) bool {
	if h == nil || h.gatewayService == nil || !h.hedgedResponsesEnabled(c, in) {
		return false
	}

	candidates := h.collectOpenAIHedgedResponsesCandidates(c, in)
	if len(candidates) < 2 {
		for _, candidate := range candidates {
			if candidate != nil && candidate.Release != nil {
				candidate.Release()
			}
		}
		return false
	}

	service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(in.RoutingStart).Milliseconds())
	forwardStart := time.Now()
	writerSizeBeforeForward := c.Writer.Size()
	coord := newOpenAIHedgedStreamCoordinator(c.Writer)
	ctx, cancelAll := context.WithCancel(context.WithoutCancel(c.Request.Context()))
	defer cancelAll()

	cancelByID := make(map[int]context.CancelFunc, len(candidates))
	runs := make([]openAIHedgedCandidateRun, 0, len(candidates))
	for _, candidate := range candidates {
		candidateCtx, candidateCancel := context.WithCancel(ctx)
		shadow := c.Copy()
		shadow.Writer = newOpenAIHedgedResponseWriter(candidate.ID, coord)
		shadow.Request = c.Request.WithContext(candidateCtx)
		service.SetOpenAIClientTransport(shadow, service.OpenAIClientTransportHTTP)
		cancelByID[candidate.ID] = candidateCancel
		runs = append(runs, openAIHedgedCandidateRun{
			Candidate: candidate,
			Context:   candidateCtx,
			Cancel:    candidateCancel,
			Shadow:    shadow,
		})
	}
	coord.SetWinnerCallback(func(winnerID int) {
		for id, cancel := range cancelByID {
			if id != winnerID && cancel != nil {
				cancel()
			}
		}
	})

	resultCh := make(chan openAIHedgedCandidateResult, len(candidates))
	for _, run := range runs {
		run := run
		go func() {
			candidate := run.Candidate
			defer run.Cancel()
			defer func() {
				if candidate.Release != nil {
					candidate.Release()
				}
			}()

			forwardCtx := service.WithCancelableOpenAIHTTPUpstream(run.Context)
			result, err := h.gatewayService.Forward(forwardCtx, run.Shadow, candidate.Account, in.ForwardBody)
			resultCh <- openAIHedgedCandidateResult{
				Candidate: candidate,
				Result:    result,
				Err:       err,
			}
		}()
	}

	var winnerResult openAIHedgedCandidateResult
	reportedLosers := make(map[int]struct{}, len(candidates))
	completed := 0
	for completed < len(candidates) {
		out := <-resultCh
		completed++
		if out.Candidate == nil || !coord.IsWinner(out.Candidate.ID) {
			if out.Candidate != nil && out.Candidate.Account != nil {
				if _, reported := reportedLosers[out.Candidate.ID]; !reported {
					h.gatewayService.ReportOpenAIAccountScheduleResult(out.Candidate.Account.ID, false, nil)
					reportedLosers[out.Candidate.ID] = struct{}{}
				}
			}
			continue
		}
		winnerResult = out
		cancelAll()
		completed = len(candidates)
	}

	if winnerResult.Candidate == nil || winnerResult.Candidate.Account == nil {
		return false
	}
	for _, candidate := range candidates {
		if candidate == nil || candidate.Account == nil || candidate.ID == winnerResult.Candidate.ID {
			continue
		}
		if _, reported := reportedLosers[candidate.ID]; reported {
			continue
		}
		h.gatewayService.ReportOpenAIAccountScheduleResult(candidate.Account.ID, false, nil)
		reportedLosers[candidate.ID] = struct{}{}
	}

	account := winnerResult.Candidate.Account
	result := winnerResult.Result
	err := winnerResult.Err
	forwardDurationMs := time.Since(forwardStart).Milliseconds()
	service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, forwardDurationMs)
	if result != nil && result.FirstTokenMs != nil {
		service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	if bindErr := h.gatewayService.BindStickySession(c.Request.Context(), in.APIKey.GroupID, in.SessionHash, account.ID); bindErr != nil && in.RequestLog != nil {
		in.RequestLog.Warn("openai.hedged.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(bindErr))
	}

	if err != nil {
		h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
		if result != nil && result.ImageCount > 0 {
			if in.RequestLog != nil {
				in.RequestLog.Warn("openai.hedged.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			}
		} else {
			if c.Writer.Size() != writerSizeBeforeForward {
				h.ensureForwardErrorResponse(c, true)
			} else {
				h.ensureForwardErrorResponse(c, false)
			}
			if in.RequestLog != nil {
				in.RequestLog.Warn("openai.hedged.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Error(err),
				)
			}
			return true
		}
	}

	if result == nil {
		h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, nil)
		return true
	}

	h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
	h.recordOpenAIResponsesUsage(c, result, in.APIKey, in.Subscription, account, in.Subject.UserID, in.RequestModel, in.Body, in.ChannelMapping, "handler.openai_gateway.responses")
	if in.RequestLog != nil {
		in.RequestLog.Info("openai.hedged.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("candidate_count", len(candidates)),
		)
	}
	return true
}

func (h *OpenAIGatewayHandler) hedgedResponsesEnabled(c *gin.Context, in openAIHedgedResponsesInput) bool {
	if h == nil || h.cfg == nil || !validOpenAIHedgedMaxParallel(h.cfg.Gateway.HedgedRequests.MaxParallel) {
		return false
	}
	if c == nil || c.Request == nil || c.Writer == nil {
		return false
	}
	if in.APIKey == nil || in.APIKey.Group == nil {
		return false
	}
	if in.APIKey.Group.Platform != service.PlatformOpenAI || !in.APIKey.Group.HedgedRequestsEnabled {
		return false
	}
	if !gjson.GetBytes(in.Body, "stream").Bool() {
		return false
	}
	if in.RequireCompact || strings.TrimSpace(in.SessionHash) != "" {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(in.Body, "previous_response_id").String()) != "" {
		return false
	}
	if service.IsImageGenerationIntent("/v1/responses", in.RequestModel, in.Body) {
		return false
	}
	if openAIHedgedBodyLooksStateful(in.Body) {
		return false
	}
	return true
}

func validOpenAIHedgedMaxParallel(maxParallel int) bool {
	return maxParallel == 0 || maxParallel >= 2
}

func openAIHedgedBodyLooksStateful(body []byte) bool {
	for _, marker := range []string{
		"function_call_output",
		"tool_search_output",
		"custom_tool_call_output",
		"mcp_tool_call_output",
		"item_reference",
	} {
		if bytes.Contains(body, []byte(marker)) {
			return true
		}
	}
	return false
}

func (h *OpenAIGatewayHandler) collectOpenAIHedgedResponsesCandidates(c *gin.Context, in openAIHedgedResponsesInput) []*openAIHedgedCandidate {
	maxParallel := h.cfg.Gateway.HedgedRequests.MaxParallel
	capacity := maxParallel
	if capacity <= 0 {
		capacity = 4
	}
	candidates := make([]*openAIHedgedCandidate, 0, capacity)
	excludedIDs := make(map[int64]struct{})
	for maxParallel == 0 || len(candidates) < maxParallel {
		selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			in.APIKey.GroupID,
			"",
			"",
			in.RequestModel,
			excludedIDs,
			service.OpenAIUpstreamTransportHTTPSSE,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
		)
		if err != nil || selection == nil || selection.Account == nil {
			break
		}

		account := selection.Account
		if _, seen := excludedIDs[account.ID]; seen {
			break
		}
		excludedIDs[account.ID] = struct{}{}
		release, acquired := h.acquireOpenAIHedgedCandidateSlot(c, selection)
		if !acquired {
			continue
		}
		if !openAIHedgedAccountEligible(account) {
			if release != nil {
				release()
			}
			continue
		}
		candidates = append(candidates, &openAIHedgedCandidate{
			ID:      len(candidates) + 1,
			Account: account,
			Release: release,
		})
	}
	return candidates
}

func (h *OpenAIGatewayHandler) acquireOpenAIHedgedCandidateSlot(c *gin.Context, selection *service.AccountSelectionResult) (func(), bool) {
	if selection == nil || selection.Account == nil {
		return nil, false
	}
	if selection.Acquired {
		return openAIHedgedReleaseOnce(selection.ReleaseFunc), true
	}
	if selection.WaitPlan == nil || h.concurrencyHelper == nil {
		return nil, false
	}
	release, acquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
		c.Request.Context(),
		selection.Account.ID,
		selection.WaitPlan.MaxConcurrency,
	)
	if err != nil || !acquired {
		return nil, false
	}
	return openAIHedgedReleaseOnce(release), true
}

func openAIHedgedAccountEligible(account *service.Account) bool {
	if account == nil || account.Type != service.AccountTypeAPIKey {
		return false
	}
	if account.IsOpenAIPassthroughEnabled() {
		return false
	}
	if account.IsPoolMode() {
		return false
	}
	return openai_compat.ShouldUseResponsesAPI(account.Extra)
}

func openAIHedgedReleaseOnce(release func()) func() {
	if release == nil {
		return nil
	}
	var once sync.Once
	return func() {
		once.Do(release)
	}
}

func (h *OpenAIGatewayHandler) recordOpenAIResponsesUsage(
	c *gin.Context,
	result *service.OpenAIForwardResult,
	apiKey *service.APIKey,
	subscription *service.UserSubscription,
	account *service.Account,
	subjectUserID int64,
	reqModel string,
	body []byte,
	channelMapping service.ChannelMappingResult,
	component string,
) {
	if h == nil || h.gatewayService == nil || c == nil || result == nil || apiKey == nil || account == nil {
		return
	}
	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	requestPayloadHash := service.HashUsageRequestPayload(body)
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
	h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             apiKey,
			User:               apiKey.User,
			Account:            account,
			Subscription:       subscription,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			RequestPayloadHash: requestPayloadHash,
			APIKeyService:      h.apiKeyService,
			ChannelUsageFields: channelMapping.ToUsageFields(reqModel, result.UpstreamModel),
		}); err != nil {
			logger.L().With(
				zap.String("component", component),
				zap.Int64("user_id", subjectUserID),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
				zap.String("model", reqModel),
				zap.Int64("account_id", account.ID),
			).Error("openai.record_usage_failed", zap.Error(err))
		}
	})
}

type openAIHedgedStreamCoordinator struct {
	mu       sync.Mutex
	real     gin.ResponseWriter
	winnerID int
	onWinner func(int)
}

func newOpenAIHedgedStreamCoordinator(real gin.ResponseWriter) *openAIHedgedStreamCoordinator {
	return &openAIHedgedStreamCoordinator{real: real}
}

func (c *openAIHedgedStreamCoordinator) SetWinnerCallback(fn func(int)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onWinner = fn
}

func (c *openAIHedgedStreamCoordinator) IsWinner(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.winnerID == id
}

func (c *openAIHedgedStreamCoordinator) hasOtherWinner(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.winnerID != 0 && c.winnerID != id
}

func (c *openAIHedgedStreamCoordinator) tryClaim(id int, header http.Header, status int, payload []byte) bool {
	var onWinner func(int)
	c.mu.Lock()
	if c.winnerID == 0 {
		c.winnerID = id
		copyHedgedResponseHeaders(c.real.Header(), header)
		if status <= 0 {
			status = http.StatusOK
		}
		if !c.real.Written() {
			c.real.WriteHeader(status)
		}
		if len(payload) > 0 {
			_, _ = c.real.Write(payload)
		}
		onWinner = c.onWinner
	} else if c.winnerID != id {
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()
	if onWinner != nil {
		onWinner(id)
	}
	return true
}

func (c *openAIHedgedStreamCoordinator) write(id int, payload []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.winnerID != id {
		return 0, context.Canceled
	}
	return c.real.Write(payload)
}

func (c *openAIHedgedStreamCoordinator) flush(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.winnerID != id {
		return
	}
	if flusher, ok := c.real.(http.Flusher); ok {
		flusher.Flush()
	}
}

func copyHedgedResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type openAIHedgedResponseWriter struct {
	id     int
	coord  *openAIHedgedStreamCoordinator
	header http.Header

	mu      sync.Mutex
	status  int
	size    int
	pending bytes.Buffer
	closed  bool
}

func newOpenAIHedgedResponseWriter(id int, coord *openAIHedgedStreamCoordinator) *openAIHedgedResponseWriter {
	return &openAIHedgedResponseWriter{
		id:     id,
		coord:  coord,
		header: make(http.Header),
		status: http.StatusOK,
		size:   -1,
	}
}

func (w *openAIHedgedResponseWriter) Header() http.Header {
	return w.header
}

func (w *openAIHedgedResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size >= 0 {
		return
	}
	if code > 0 {
		w.status = code
	}
}

func (w *openAIHedgedResponseWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.coord.IsWinner(w.id) && w.size < 0 {
		w.size = 0
	}
}

func (w *openAIHedgedResponseWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, context.Canceled
	}
	if w.coord.hasOtherWinner(w.id) {
		w.closed = true
		w.mu.Unlock()
		return 0, context.Canceled
	}
	if w.coord.IsWinner(w.id) {
		w.mu.Unlock()
		n, err := w.coord.write(w.id, data)
		if err != nil {
			return n, err
		}
		w.addSize(n)
		return n, nil
	}
	_, _ = w.pending.Write(data)
	w.mu.Unlock()
	return len(data), nil
}

func (w *openAIHedgedResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *openAIHedgedResponseWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *openAIHedgedResponseWriter) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

func (w *openAIHedgedResponseWriter) Written() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size != -1
}

func (w *openAIHedgedResponseWriter) Flush() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if w.coord.IsWinner(w.id) {
		w.mu.Unlock()
		w.coord.flush(w.id)
		return
	}
	if w.coord.hasOtherWinner(w.id) {
		w.closed = true
		w.mu.Unlock()
		return
	}
	if !openAIHedgedPayloadHasClientOutput(w.pending.Bytes()) {
		w.mu.Unlock()
		return
	}
	payload := append([]byte(nil), w.pending.Bytes()...)
	header := cloneHTTPHeader(w.header)
	status := w.status
	w.mu.Unlock()

	if !w.coord.tryClaim(w.id, header, status, payload) {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		return
	}

	w.mu.Lock()
	w.pending.Reset()
	if w.size < 0 {
		w.size = 0
	}
	w.size += len(payload)
	w.mu.Unlock()
	w.coord.flush(w.id)
}

func (w *openAIHedgedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hedged response writer does not support hijacking")
}

func (w *openAIHedgedResponseWriter) CloseNotify() <-chan bool {
	ch := make(chan bool)
	return ch
}

func (w *openAIHedgedResponseWriter) Pusher() http.Pusher {
	return nil
}

func (w *openAIHedgedResponseWriter) addSize(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size < 0 {
		w.size = 0
	}
	w.size += n
}

func cloneHTTPHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func openAIHedgedPayloadHasClientOutput(payload []byte) bool {
	for _, rawLine := range bytes.Split(payload, []byte{'\n'}) {
		line := strings.TrimSpace(string(rawLine))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		eventType := strings.TrimSpace(gjson.Get(data, "type").String())
		switch eventType {
		case "response.created", "response.in_progress", "response.failed", "error":
			continue
		default:
			return true
		}
	}
	return false
}

var _ gin.ResponseWriter = (*openAIHedgedResponseWriter)(nil)
