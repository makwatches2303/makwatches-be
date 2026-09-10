package handlers

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
)

// rateLimit builds a per-client rate limiter keyed on the real client IP.
//
// c.IP() alone returns whichever peer terminated the TCP connection, which
// behind a reverse proxy (Railway, and most production deployments) is the
// proxy itself for every request -- keying on that would either rate-limit
// every visitor as one shared client or do nothing useful. X-Forwarded-For's
// first entry is the original client when the proxy sets one; c.IP() is the
// fallback for direct/local connections.
func rateLimit(max int, window time.Duration) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        max,
		Expiration: window,
		KeyGenerator: func(c *fiber.Ctx) string {
			if fwd := c.Get("X-Forwarded-For"); fwd != "" {
				if i := strings.IndexByte(fwd, ','); i >= 0 {
					return strings.TrimSpace(fwd[:i])
				}
				return strings.TrimSpace(fwd)
			}
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"success": false,
				"message": "Too many requests. Please try again shortly.",
			})
		},
	})
}
