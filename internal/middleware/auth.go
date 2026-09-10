package middleware

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/shivam-mishra-20/mak-watches-be/internal/debuglog"
)

// TokenMetadata contains user metadata from the JWT token
type TokenMetadata struct {
	UserID primitive.ObjectID
	Role   string
	Exp    time.Time
}

// Auth middleware for protecting routes
func Auth(jwtSecret string) fiber.Handler {
    return func(c *fiber.Ctx) error {
        tokenHeader := c.Get("Authorization")
        if tokenHeader == "" {
            // Log the request details for debugging
            debuglog.Printf("[AUTH] Missing Authorization header - Method: %s, Path: %s, IP: %s\n",
                c.Method(), c.Path(), c.IP())
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Authorization header is required",
            })
        }

        // Check if the header format is correct
        parts := strings.Split(tokenHeader, " ")
        if len(parts) != 2 || parts[0] != "Bearer" {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Authorization header format must be Bearer {token}",
            })
        }

        tokenString := parts[1]

        // Parse the JWT token
        token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
            // Validate signing method
            if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
                return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
            }
            return []byte(jwtSecret), nil
        })

        if err != nil {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Invalid or expired token",
            })
        }

        // Validate token
        if !token.Valid {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Invalid token",
            })
        }

        // Extract claims
        claims, ok := token.Claims.(jwt.MapClaims)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Failed to extract claims from token",
            })
        }

        // Verify expiration
        expFloat, ok := claims["exp"].(float64)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Invalid token expiration",
            })
        }

        expTime := time.Unix(int64(expFloat), 0)
        if time.Now().After(expTime) {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Token has expired",
            })
        }

        // Extract user ID
        userIDStr, ok := claims["userId"].(string)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Invalid user ID in token",
            })
        }

        userID, err := primitive.ObjectIDFromHex(userIDStr)
        if err != nil {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Invalid user ID format",
            })
        }

        // Extract role
        role, ok := claims["role"].(string)
        if !ok {
            role = "user" // Default role
        }

        // Set user metadata in context
        c.Locals("user", &TokenMetadata{
            UserID: userID,
            Role:   role,
            Exp:    expTime,
        })

        // Log successful authentication
        debuglog.Printf("[AUTH] User authenticated - UserID: %s, Role: %s, Path: %s\n",
            userID.Hex(), role, c.Path())

        return c.Next()
    }
}

// Role middleware for checking user roles
func Role(roles ...string) fiber.Handler {
    return func(c *fiber.Ctx) error {
        // Get user metadata from context
        user, ok := c.Locals("user").(*TokenMetadata)
        if !ok {
            return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
                "success": false,
                "message": "Unauthorized - User data not found",
            })
        }

        // Check if user role is allowed
        allowed := false
        for _, role := range roles {
            if user.Role == role {
                allowed = true
                break
            }
        }

        if !allowed {
            return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
                "success": false,
                "message": "Access forbidden - Insufficient permissions",
            })
        }

        return c.Next()
    }
}