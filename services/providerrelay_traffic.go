package services

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

const trafficInboundStateKey = "codeswitch_traffic_inbound_state"

type byteCountingReadCloser struct {
	io.ReadCloser
	count   *atomic.Int64
	once    sync.Once
	onClose func()
}

func (r *byteCountingReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if r.count != nil && n > 0 {
		r.count.Add(int64(n))
	}
	return n, err
}

func (r *byteCountingReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() {
		if r.onClose != nil {
			r.onClose()
		}
	})
	return err
}

type trafficInboundState struct {
	requestBytes atomic.Int64
	writer       gin.ResponseWriter
	traceID      string
	platform     string
	scope        string
}

func (prs *ProviderRelayService) trafficMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		state := &trafficInboundState{writer: c.Writer}
		if c.Request.Body != nil {
			c.Request.Body = &byteCountingReadCloser{ReadCloser: c.Request.Body, count: &state.requestBytes}
		}
		c.Set(trafficInboundStateKey, state)
		c.Next()

		if prs == nil || prs.trafficService == nil {
			return
		}
		clientIP := clientIPFromRequest(c.Request)
		scope := state.scope
		if scope == "" {
			scope = ClassifyNetworkScope(clientIP)
		}
		responseBytes := int64(c.Writer.Size())
		if responseBytes < 0 {
			responseBytes = 0
		}
		prs.trafficService.Record(TrafficEvent{
			TraceID:       state.traceID,
			UserID:        relayUserIDFromContext(c),
			Category:      TrafficCategoryRelayClient,
			Route:         c.Request.URL.Path,
			Platform:      state.platform,
			NetworkScope:  scope,
			RequestBytes:  state.requestBytes.Load(),
			ResponseBytes: responseBytes,
			StatusCode:    c.Writer.Status(),
		})
	}
}

type requestTrafficState struct {
	mu           sync.Mutex
	inbound      *trafficInboundState
	attemptIndex int
}

func (r *ReqeustLog) initializeTraffic(c *gin.Context) {
	if r == nil {
		return
	}
	state := &requestTrafficState{}
	if c != nil {
		if value, ok := c.Get(trafficInboundStateKey); ok {
			state.inbound, _ = value.(*trafficInboundState)
		}
	}
	if r.TrafficTraceID == "" {
		r.TrafficTraceID = NewTrafficTraceID()
	}
	r.ClientNetworkScope = ClassifyNetworkScope(r.ClientIP)
	if state.inbound != nil {
		state.inbound.traceID = r.TrafficTraceID
		state.inbound.platform = r.Platform
		state.inbound.scope = r.ClientNetworkScope
	}
	r.traffic = state
}

func (r *ReqeustLog) finalizeClientTraffic() {
	if r == nil || r.traffic == nil || r.traffic.inbound == nil {
		return
	}
	state := r.traffic.inbound
	r.traffic.mu.Lock()
	defer r.traffic.mu.Unlock()
	r.ClientRequestBytes = state.requestBytes.Load()
	if state.writer != nil {
		r.ClientResponseBytes = int64(state.writer.Size())
		if r.ClientResponseBytes < 0 {
			r.ClientResponseBytes = 0
		}
	}
	if r.ClientNetworkScope == TrafficScopeLocal {
		r.LocalIngressBytes += r.ClientRequestBytes
		r.LocalEgressBytes += r.ClientResponseBytes
	} else {
		r.PublicIngressBytes += r.ClientRequestBytes
		r.PublicEgressBytes += r.ClientResponseBytes
	}
}

type upstreamTrafficMetadata struct {
	service    *TrafficService
	requestLog *ReqeustLog
	provider   string
	targetURL  string
}

type upstreamTrafficAttempt struct {
	metadata      *upstreamTrafficMetadata
	index         int
	retry         bool
	scope         string
	requestBytes  atomic.Int64
	responseBytes atomic.Int64
	statusCode    atomic.Int64
	finishOnce    sync.Once
}

func (metadata *upstreamTrafficMetadata) begin() *upstreamTrafficAttempt {
	if metadata == nil || metadata.requestLog == nil {
		return nil
	}
	logEntry := metadata.requestLog
	if logEntry.traffic == nil {
		logEntry.traffic = &requestTrafficState{}
	}
	logEntry.traffic.mu.Lock()
	logEntry.traffic.attemptIndex++
	index := logEntry.traffic.attemptIndex
	logEntry.UpstreamAttempts = index
	logEntry.traffic.mu.Unlock()
	return &upstreamTrafficAttempt{
		metadata: metadata,
		index:    index,
		retry:    index > 1,
		scope:    ClassifyNetworkScope(metadata.targetURL),
	}
}

func (attempt *upstreamTrafficAttempt) wrapRequest(request *http.Request) {
	if attempt == nil || request == nil || request.Body == nil {
		return
	}
	request.Body = &byteCountingReadCloser{ReadCloser: request.Body, count: &attempt.requestBytes}
	if request.GetBody != nil {
		originalGetBody := request.GetBody
		request.GetBody = func() (io.ReadCloser, error) {
			body, err := originalGetBody()
			if err != nil {
				return nil, err
			}
			return &byteCountingReadCloser{ReadCloser: body, count: &attempt.requestBytes}, nil
		}
	}
}

func (attempt *upstreamTrafficAttempt) wrapResponse(response *http.Response) {
	if attempt == nil {
		return
	}
	if response != nil {
		attempt.statusCode.Store(int64(response.StatusCode))
	}
	if response == nil || response.Body == nil {
		attempt.finish()
		return
	}
	response.Body = &byteCountingReadCloser{
		ReadCloser: response.Body,
		count:      &attempt.responseBytes,
		onClose:    attempt.finish,
	}
}

func (attempt *upstreamTrafficAttempt) finish() {
	if attempt == nil || attempt.metadata == nil || attempt.metadata.requestLog == nil {
		return
	}
	attempt.finishOnce.Do(func() {
		requestBytes := attempt.requestBytes.Load()
		responseBytes := attempt.responseBytes.Load()
		requestLog := attempt.metadata.requestLog
		if requestLog.traffic == nil {
			requestLog.traffic = &requestTrafficState{}
		}
		requestLog.traffic.mu.Lock()
		requestLog.UpstreamRequestBytes += requestBytes
		requestLog.UpstreamResponseBytes += responseBytes
		if attempt.retry {
			requestLog.RetryRequestBytes += requestBytes
			requestLog.RetryResponseBytes += responseBytes
		}
		if attempt.scope == TrafficScopeLocal {
			requestLog.LocalEgressBytes += requestBytes
			requestLog.LocalIngressBytes += responseBytes
		} else {
			requestLog.PublicEgressBytes += requestBytes
			requestLog.PublicIngressBytes += responseBytes
		}
		requestLog.traffic.mu.Unlock()

		if attempt.metadata.service != nil {
			attempt.metadata.service.Record(TrafficEvent{
				TraceID:       requestLog.TrafficTraceID,
				UserID:        requestLog.UserID,
				Category:      TrafficCategoryRelayUpstream,
				Route:         trafficURLPath(attempt.metadata.targetURL),
				Platform:      requestLog.Platform,
				Provider:      attempt.metadata.provider,
				AttemptIndex:  attempt.index,
				Retry:         attempt.retry,
				NetworkScope:  attempt.scope,
				RequestBytes:  requestBytes,
				ResponseBytes: responseBytes,
				StatusCode:    int(attempt.statusCode.Load()),
			})
		}
	})
}

func trafficURLPath(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}
