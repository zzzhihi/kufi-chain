// Package api provides middleware for HTTP handlers
package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/fabric-payment-gateway/internal/config"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// LoggingMiddleware provides request/response logging
func LoggingMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		// Process request
		c.Next()

		// Log after request completed
		latency := time.Since(start)
		status := c.Writer.Status()

		logger.Info("HTTP request",
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.String("query", query),
			zap.Int("status", status),
			zap.Duration("latency", latency),
			zap.String("client_ip", c.ClientIP()),
			zap.String("user_agent", c.Request.UserAgent()),
		)
	}
}

// AuditMiddleware provides audit logging for compliance
func AuditMiddleware(logger *zap.Logger, maskFields []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Read and restore request body for audit
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		// Compute body hash for audit trail
		bodyHash := ""
		if len(bodyBytes) > 0 {
			hash := sha256.Sum256(bodyBytes)
			bodyHash = hex.EncodeToString(hash[:])
		}

		// Store audit info in context
		c.Set("audit_body_hash", bodyHash)
		c.Set("audit_timestamp", time.Now().UnixMilli())

		// Process request
		c.Next()

		// Log audit entry after request
		logger.Info("AUDIT",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.String("client_ip", c.ClientIP()),
			zap.String("body_hash", bodyHash),
			zap.Int("status", c.Writer.Status()),
			zap.Int64("timestamp", time.Now().UnixMilli()),
		)
	}
}

// RecoveryMiddleware handles panics gracefully
func RecoveryMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("Panic recovered",
					zap.Any("error", err),
					zap.String("path", c.Request.URL.Path),
				)

				c.JSON(500, TransferResponse{
					Success: false,
					Error: &ErrorDetail{
						Code:    ErrCodeInternalError,
						Message: "Internal server error",
					},
					ProcessedAt: time.Now().UnixMilli(),
				})
				c.Abort()
			}
		}()
		c.Next()
	}
}

// TimeoutMiddleware adds request timeout
func TimeoutMiddleware(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Note: For proper timeout handling, use context.WithTimeout
		// This middleware sets a deadline header for clients
		c.Header("X-Request-Timeout", timeout.String())
		c.Next()
	}
}

// RateLimitMiddleware provides basic rate limiting
// In production, use a distributed rate limiter (Redis-based)
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

// RateLimitMiddleware returns gin middleware for rate limiting
func (rl *RateLimiter) RateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		clientIP := c.ClientIP()

		rl.mu.Lock()

		// Clean old requests
		now := time.Now()
		cutoff := now.Add(-rl.window)

		requests := rl.requests[clientIP]
		validRequests := make([]time.Time, 0, len(requests))
		for _, t := range requests {
			if t.After(cutoff) {
				validRequests = append(validRequests, t)
			}
		}

		// Check limit
		if len(validRequests) >= rl.limit {
			rl.mu.Unlock()
			c.JSON(http.StatusTooManyRequests, TransferResponse{
				Success: false,
				Error: &ErrorDetail{
					Code:    "RATE_LIMITED",
					Message: "Too many requests",
				},
				ProcessedAt: time.Now().UnixMilli(),
			})
			c.Abort()
			return
		}

		// Record this request
		validRequests = append(validRequests, now)
		rl.requests[clientIP] = validRequests
		rl.mu.Unlock()

		c.Next()
	}
}

// CORSMiddleware adds CORS headers
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Idempotency-Key")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// RequestIDMiddleware adds a unique request ID to each request
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if request ID provided by client
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			// Generate cryptographically secure request ID
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			requestID = fmt.Sprintf("%x-%x-%x-%x-%x",
				b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		}

		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)

		c.Next()
	}
}

// APIKeyMiddleware validates API key authentication
func APIKeyMiddleware(apiKeyCfg config.APIKeyConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !apiKeyCfg.Enabled {
			c.Next()
			return
		}

		apiKey := c.GetHeader("X-API-Key")
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, TransferResponse{
				Success: false,
				Error: &ErrorDetail{
					Code:    "UNAUTHORIZED",
					Message: "Missing API key",
				},
				ProcessedAt: time.Now().UnixMilli(),
			})
			c.Abort()
			return
		}

		// Constant-time comparison to prevent timing attacks
		validKey := false
		for _, key := range apiKeyCfg.Keys {
			if subtle.ConstantTimeCompare([]byte(apiKey), []byte(key)) == 1 {
				validKey = true
				break
			}
		}

		if !validKey {
			c.JSON(http.StatusForbidden, TransferResponse{
				Success: false,
				Error: &ErrorDetail{
					Code:    "FORBIDDEN",
					Message: "Invalid API key",
				},
				ProcessedAt: time.Now().UnixMilli(),
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// SecurityHeadersMiddleware adds security headers
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		c.Header("Content-Security-Policy", "default-src 'self'")
		c.Next()
	}
}
