package main

import (
	"io"
	"sync/atomic"

	"codeswitch/services"
	"github.com/gin-gonic/gin"
)

const adminTrafficRouteContextKey = "admin_traffic_route"

type adminTrafficCountingBody struct {
	io.ReadCloser
	bytes atomic.Int64
}

func (body *adminTrafficCountingBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	if n > 0 {
		body.bytes.Add(int64(n))
	}
	return n, err
}

func adminTrafficMiddleware(rt *appRuntime) gin.HandlerFunc {
	return func(c *gin.Context) {
		var requestBody *adminTrafficCountingBody
		if c.Request.Body != nil {
			requestBody = &adminTrafficCountingBody{ReadCloser: c.Request.Body}
			c.Request.Body = requestBody
		}
		c.Next()
		if rt == nil || rt.trafficService == nil {
			return
		}
		route := c.Request.URL.Path
		if value, ok := c.Get(adminTrafficRouteContextKey); ok {
			if rpcName, ok := value.(string); ok && rpcName != "" {
				route = rpcName
			}
		}
		clientIP := ""
		if rt.adminSecurity != nil {
			if address := rt.adminSecurity.actualClientIP(c.Request); address.IsValid() {
				clientIP = address.String()
			}
		}
		requestBytes := int64(0)
		if requestBody != nil {
			requestBytes = requestBody.bytes.Load()
		}
		responseBytes := int64(c.Writer.Size())
		if responseBytes < 0 {
			responseBytes = 0
		}
		rt.trafficService.Record(services.TrafficEvent{
			UserID:        adminUserIDFromContext(c),
			Category:      services.TrafficCategoryAdmin,
			Route:         route,
			NetworkScope:  services.ClassifyNetworkScope(clientIP),
			RequestBytes:  requestBytes,
			ResponseBytes: responseBytes,
			StatusCode:    c.Writer.Status(),
		})
	}
}
