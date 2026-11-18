package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
)

// Audit 注入 request_id 便于全链路追踪。
// 管理审计由各 controller 显式调用 audit.Record 落库，本中间件**不写 admin_audit**（避免与 controller 重复记录）。
func Audit() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := uuid.NewString()
		c.Set("request_id", requestID)
		c.Header("X-Request-Id", requestID)

		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			logger.WithRequestID(requestID).Info("管理写操作",
				zap.String("method", c.Request.Method),
				zap.String("path", c.FullPath()),
			)
		}
		c.Next()
	}
}
