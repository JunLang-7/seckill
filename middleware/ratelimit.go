package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// GlobalRateLimiter a global rate limiter of token bucket
// ratePerSec represents token generated per sec
// burstCapacity represents how mush burst traffic can be handled
func GlobalRateLimiter(ratePerSec float64, burstCapacity int) gin.HandlerFunc {
	// 1. Initial a global limiter
	limiter := rate.NewLimiter(rate.Limit(ratePerSec), burstCapacity)

	return func(c *gin.Context) {
		// 2. When request comes, acquire for a token from bucket
		// Allow() non-blocking，return true if retrieved，return false when bucket is empty
		if !limiter.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			c.Abort()
			return
		}
		c.Next()
	}
}
