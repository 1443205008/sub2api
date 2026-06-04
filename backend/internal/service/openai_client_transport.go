package service

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
)

// OpenAIClientTransport 表示客户端入站协议类型。
type OpenAIClientTransport string

const (
	OpenAIClientTransportUnknown OpenAIClientTransport = ""
	OpenAIClientTransportHTTP    OpenAIClientTransport = "http"
	OpenAIClientTransportWS      OpenAIClientTransport = "ws"
)

const openAIClientTransportContextKey = "openai_client_transport"

type openAICancelableHTTPUpstreamContextKey struct{}

// SetOpenAIClientTransport 标记当前请求的客户端入站协议。
func SetOpenAIClientTransport(c *gin.Context, transport OpenAIClientTransport) {
	if c == nil {
		return
	}
	normalized := normalizeOpenAIClientTransport(transport)
	if normalized == OpenAIClientTransportUnknown {
		return
	}
	c.Set(openAIClientTransportContextKey, string(normalized))
}

// GetOpenAIClientTransport 读取当前请求的客户端入站协议。
func GetOpenAIClientTransport(c *gin.Context) OpenAIClientTransport {
	if c == nil {
		return OpenAIClientTransportUnknown
	}
	raw, ok := c.Get(openAIClientTransportContextKey)
	if !ok || raw == nil {
		return OpenAIClientTransportUnknown
	}

	switch v := raw.(type) {
	case OpenAIClientTransport:
		return normalizeOpenAIClientTransport(v)
	case string:
		return normalizeOpenAIClientTransport(OpenAIClientTransport(v))
	default:
		return OpenAIClientTransportUnknown
	}
}

func normalizeOpenAIClientTransport(transport OpenAIClientTransport) OpenAIClientTransport {
	switch strings.ToLower(strings.TrimSpace(string(transport))) {
	case string(OpenAIClientTransportHTTP), "http_sse", "sse":
		return OpenAIClientTransportHTTP
	case string(OpenAIClientTransportWS), "websocket":
		return OpenAIClientTransportWS
	default:
		return OpenAIClientTransportUnknown
	}
}

func resolveOpenAIWSDecisionByClientTransport(
	decision OpenAIWSProtocolDecision,
	clientTransport OpenAIClientTransport,
) OpenAIWSProtocolDecision {
	if clientTransport == OpenAIClientTransportHTTP {
		return openAIWSHTTPDecision("client_protocol_http")
	}
	return decision
}

// WithCancelableOpenAIHTTPUpstream marks a request whose HTTP upstream should
// stay tied to the caller's context. Normal streaming requests intentionally
// detach from the downstream client so billing can drain usage after a client
// disconnects; hedged loser requests need the opposite behavior so they can be
// canceled as soon as another upstream wins.
func WithCancelableOpenAIHTTPUpstream(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAICancelableHTTPUpstreamContextKey{}, true)
}

func useCancelableOpenAIHTTPUpstream(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(openAICancelableHTTPUpstreamContextKey{}).(bool)
	return enabled
}

func openAIHTTPUpstreamContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if useCancelableOpenAIHTTPUpstream(ctx) {
		return ctx, func() {}
	}
	return detachUpstreamContext(ctx)
}
