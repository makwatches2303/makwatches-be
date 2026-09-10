package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/crypto/bcrypt"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
	"github.com/shivam-mishra-20/mak-watches-be/pkg/utils"
)

// AuthHandler handles authentication related requests
type AuthHandler struct {
	DB          *database.DBClient
	Config      *config.Config
	GoogleOAuth *utils.GoogleOAuth
}

// NewAuthHandler creates a new instance of AuthHandler
func NewAuthHandler(db *database.DBClient, cfg *config.Config) *AuthHandler {
	googleOAuth := utils.NewGoogleOAuth(
		cfg.GoogleClientID,
		cfg.GoogleClientSecret,
		cfg.GoogleRedirectURL,
	)

	ensureOAuthTempIndex(context.Background(), db.MongoDB)

	return &AuthHandler{
		DB:          db,
		Config:      cfg,
		GoogleOAuth: googleOAuth,
	}
}

// Register handles user registration
func (h *AuthHandler) Register(c *fiber.Ctx) error {
	ctx := c.Context()
	var req models.RegisterRequest

	// Parse request body
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// Validate required fields
	if req.Name == "" || req.Email == "" || req.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Name, email, and password are required",
		})
	}

	// Check if user already exists
	collection := h.DB.Collections().Users
	var existingUser models.User
	err := collection.FindOne(ctx, bson.M{"email": req.Email}).Decode(&existingUser)
	if err == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "User with this email already exists",
		})
	} else if err != mongo.ErrNoDocuments {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Database error",
			"error":   err.Error(),
		})
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to hash password",
			"error":   err.Error(),
		})
	}

	// Create new user
	now := time.Now()
	// Role is never taken from the request: /auth/register is public, and an
	// admin role must only ever be granted by an existing admin. Every
	// self-registered account is a "user" account, full stop.
	newUser := models.User{
		ID:           primitive.NewObjectID(),
		Name:         req.Name,
		Email:        req.Email,
		Password:     string(hashedPassword),
		Role:         "user",
		AuthProvider: "local", // Local authentication
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	// Insert user into database
	_, err = collection.InsertOne(ctx, newUser)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to create user",
			"error":   err.Error(),
		})
	}

	// Generate JWT token
	token, err := h.generateToken(newUser.ID.Hex(), newUser.Role)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to generate token",
			"error":   err.Error(),
		})
	}

	// Return user info and token
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"message": "User registered successfully",
		"data": models.LoginResponse{
			User: models.UserResponse{
				ID:           newUser.ID,
				Name:         newUser.Name,
				Email:        newUser.Email,
				Role:         newUser.Role,
				AuthProvider: newUser.AuthProvider,
			},
			Token: token,
		},
	})
}

// Login handles user login
func (h *AuthHandler) Login(c *fiber.Ctx) error {
	ctx := c.Context()
	var req models.LoginRequest

	// Parse request body
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// Validate required fields
	if req.Email == "" || req.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Email and password are required",
		})
	}

	// Find user by email
	collection := h.DB.Collections().Users
	var user models.User
	err := collection.FindOne(ctx, bson.M{"email": req.Email}).Decode(&user)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"success": false,
				"message": "Invalid email or password",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Database error",
			"error":   err.Error(),
		})
	}

	// Check if user is using Google auth and trying to login with password
	if user.AuthProvider == "google" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "This account uses Google authentication. Please sign in with Google.",
		})
	}

	// Compare password
	err = bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password))
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Invalid email or password",
		})
	}

	// Generate JWT token
	token, err := h.generateToken(user.ID.Hex(), user.Role)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to generate token",
			"error":   err.Error(),
		})
	}

	// Generate refresh token
	refreshToken, err := h.generateRefreshToken(user.ID.Hex())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to generate refresh token",
			"error":   err.Error(),
		})
	}

	// Set refresh token in HTTP-only cookie
	c.Cookie(&fiber.Cookie{
		Name:     "refresh_token",
		Value:    refreshToken,
		Expires:  time.Now().Add(7 * 24 * time.Hour), // 7 days
		HTTPOnly: true,
		Secure:   true, // set to true in production
		SameSite: "Strict",
	})

	// Return user info and token
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Login successful",
		"data": models.LoginResponse{
			User: models.UserResponse{
				ID:           user.ID,
				Name:         user.Name,
				Email:        user.Email,
				Role:         user.Role,
				Picture:      user.Picture,
				AuthProvider: user.AuthProvider,
			},
			Token: token,
		},
	})
}

// generateRandomToken returns a cryptographically random hex string n bytes
// long -- used for both the OAuth CSRF state and the post-login exchange
// code, neither of which should be guessable the way a nanosecond timestamp
// is.
func generateRandomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func oauthStateCacheKey(state string) string   { return "oauth_state:" + state }
func oauthExchangeCacheKey(code string) string { return "oauth_exchange:" + code }

func isAllowedFrontendOrigin(origin string, configuredFrontend string) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	if host == "makwatches.in" || host == "www.makwatches.in" || strings.HasSuffix(host, ".makwatches.in") ||
		host == "localhost:3000" || host == "127.0.0.1:3000" || host == "localhost:4200" ||
		strings.HasSuffix(host, ".vercel.app") {
		return true
	}
	if configuredFrontend != "" {
		if cu, err := url.Parse(configuredFrontend); err == nil && cu.Host == u.Host {
			return true
		}
	}
	return false
}

// GoogleLogin initiates Google OAuth login
func (h *AuthHandler) GoogleLogin(c *fiber.Ctx) error {
	ctx := c.Context()

	// Capture the initiating frontend origin to preserve where the user came from
	origin := c.Query("origin")
	if origin == "" {
		if ref := c.Get("Referer"); ref != "" {
			if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
				origin = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
			}
		}
	}
	if !isAllowedFrontendOrigin(origin, h.Config.FrontendURL) {
		origin = h.Config.FrontendURL
	}
	if origin == "" || (h.Config.Environment == "production" && strings.Contains(origin, "localhost")) {
		origin = "https://makwatches.in"
	}

	// Generate a random state token to prevent CSRF, and store it in Mongo
	// (see oauth_store.go) along with the origin rather than in process memory.
	state, err := generateRandomToken(24)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to start Google sign-in",
		})
	}
	if err := oauthTempSet(ctx, h.DB.MongoDB, oauthStateCacheKey(state), origin, 10*time.Minute); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"success": false,
			"message": "Google sign-in is temporarily unavailable",
		})
	}

	// Redirect to Google's OAuth page
	authURL := h.GoogleOAuth.GetAuthURL(state)
	return c.Redirect(authURL)
}

// GoogleCallback handles the callback from Google OAuth
func (h *AuthHandler) GoogleCallback(c *fiber.Ctx) error {
	ctx := c.Context()

	// Extract code and state from query params
	code := c.Query("code")
	state := c.Query("state")

	// For debugging
	fmt.Printf("Received state: %s\n", state)

	// Validate state against the Mongo-backed store from GoogleLogin, and
	// consume it either way -- a state token is only ever good for one callback.
	var stateFound bool
	frontendURL := h.Config.FrontendURL
	if state != "" {
		var savedOrigin string
		savedOrigin, stateFound = oauthTempConsume(ctx, h.DB.MongoDB, oauthStateCacheKey(state))
		if savedOrigin != "" && isAllowedFrontendOrigin(savedOrigin, h.Config.FrontendURL) {
			frontendURL = savedOrigin
		}
	}
	if frontendURL == "" || (h.Config.Environment == "production" && strings.Contains(frontendURL, "localhost")) {
		frontendURL = "https://makwatches.in"
	}

	// Check for code parameter
	if code == "" {
		redirectErr := url.QueryEscape("missing_code")
		return c.Redirect(fmt.Sprintf("%s/auth/callback?error=%s", frontendURL, redirectErr))
	}

	if state == "" || !stateFound {
		fmt.Printf("State validation failed for state: %s\n", state)
		redirectErr := url.QueryEscape("invalid_state")
		return c.Redirect(fmt.Sprintf("%s/auth/callback?error=%s", frontendURL, redirectErr))
	}

	// Exchange code for token
	accessToken, err := h.GoogleOAuth.Exchange(code)
	if err != nil {
		// Log detailed error and redirect to frontend with an error token
		fmt.Printf("Google token exchange failed: %v\n", err)
		// Include a short encoded error message so frontend can show it
		redirectErr := url.QueryEscape("token_exchange_failed")
		return c.Redirect(fmt.Sprintf("%s/auth/callback?error=%s", frontendURL, redirectErr))
	}

	// Get user info from Google
	userInfo, err := h.GoogleOAuth.GetUserInfo(accessToken)
	if err != nil {
		fmt.Printf("Google GetUserInfo failed: %v\n", err)
		redirectErr := url.QueryEscape("userinfo_failed")
		return c.Redirect(fmt.Sprintf("%s/auth/callback?error=%s", frontendURL, redirectErr))
	}

	// Safely extract user details (handle bool vs *bool and different underlying types)
	getStr := func(key string) string {
		if v, ok := userInfo[key]; ok && v != nil {
			switch s := v.(type) {
			case string:
				return s
			case []byte:
				return string(s)
			case fmt.Stringer:
				return s.String()
			default:
				return fmt.Sprintf("%v", v)
			}
		}
		return ""
	}
	getBool := func(key string) bool {
		if v, ok := userInfo[key]; ok && v != nil {
			switch b := v.(type) {
			case bool:
				return b
			case *bool:
				return *b
			case string:
				return strings.EqualFold(b, "true")
			default:
				return false
			}
		}
		return false
	}

	googleUser := models.GoogleUser{
		ID:            getStr("id"),
		Email:         getStr("email"),
		VerifiedEmail: getBool("verified_email"),
		Name:          getStr("name"),
		Picture:       getStr("picture"),
	}

	if !googleUser.VerifiedEmail {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Email not verified by Google",
		})
	}

	// Check if user exists in our database
	collection := h.DB.Collections().Users
	var user models.User

	// First try to find by Google ID
	err = collection.FindOne(ctx, bson.M{"google_id": googleUser.ID}).Decode(&user)
	if err == mongo.ErrNoDocuments {
		// If not found by Google ID, try by email
		err = collection.FindOne(ctx, bson.M{"email": googleUser.Email}).Decode(&user)
		if err == mongo.ErrNoDocuments {
			// User doesn't exist, create a new one
			now := time.Now()
			newUser := models.User{
				ID:           primitive.NewObjectID(),
				Name:         googleUser.Name,
				Email:        googleUser.Email,
				GoogleID:     googleUser.ID,
				Picture:      googleUser.Picture,
				Role:         "user", // Default role
				AuthProvider: "google",
				CreatedAt:    now,
				UpdatedAt:    now,
			}

			_, err = collection.InsertOne(ctx, newUser)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"success": false,
					"message": "Failed to create user",
					"error":   err.Error(),
				})
			}

			user = newUser
		} else if err != nil {
			// Database error
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Database error",
				"error":   err.Error(),
			})
		} else {
			// User exists but doesn't have Google ID, update it
			if user.AuthProvider == "" || user.AuthProvider == "local" {
				update := bson.M{
					"$set": bson.M{
						"google_id":     googleUser.ID,
						"picture":       googleUser.Picture,
						"auth_provider": "hybrid", // User has both local and Google auth
						"updated_at":    time.Now(),
					},
				}

				_, err = collection.UpdateOne(ctx, bson.M{"_id": user.ID}, update)
				if err != nil {
					return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
						"success": false,
						"message": "Failed to update user",
						"error":   err.Error(),
					})
				}

				// Update local user object
				user.GoogleID = googleUser.ID
				user.Picture = googleUser.Picture
				user.AuthProvider = "hybrid"
			}
		}
	} else if err != nil {
		// Database error
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Database error",
			"error":   err.Error(),
		})
	} else {
		// User found by Google ID, update picture if needed
		if user.Picture != googleUser.Picture {
			update := bson.M{
				"$set": bson.M{
					"picture":    googleUser.Picture,
					"updated_at": time.Now(),
				},
			}

			_, err = collection.UpdateOne(ctx, bson.M{"_id": user.ID}, update)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"success": false,
					"message": "Failed to update user picture",
					"error":   err.Error(),
				})
			}

			// Update local user object
			user.Picture = googleUser.Picture
		}
	}

	// Generate JWT token
	token, err := h.generateToken(user.ID.Hex(), user.Role)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to generate token",
			"error":   err.Error(),
		})
	}

	// Hand back a short-lived, single-use exchange code instead of the JWT
	// itself. A token embedded directly in a redirect URL ends up in browser
	// history, server access logs, and any Referer header the browser sends
	// onward. The frontend calls POST /auth/exchange (ExchangeOAuthCode
	// below) with this code to get the real token back over a request body
	// instead; the code is deleted on first use.
	exchangeCode, err := generateRandomToken(24)
	if err != nil || oauthTempSet(ctx, h.DB.MongoDB, oauthExchangeCacheKey(exchangeCode), token, 60*time.Second) != nil {
		redirectErr := url.QueryEscape("token_exchange_failed")
		return c.Redirect(fmt.Sprintf("%s/auth/callback?error=%s", frontendURL, redirectErr))
	}

	return c.Redirect(fmt.Sprintf("%s/auth/callback?token=%s&code=%s", frontendURL, token, exchangeCode))
}

// ExchangeOAuthCode redeems the short-lived code GoogleCallback hands the
// frontend for the JWT it was issued alongside. The code is deleted on
// first use, so a replayed or expired code is rejected.
func (h *AuthHandler) ExchangeOAuthCode(c *fiber.Ctx) error {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.BodyParser(&req); err != nil || req.Code == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Missing exchange code",
		})
	}

	ctx := c.Context()
	token, ok := oauthTempConsume(ctx, h.DB.MongoDB, oauthExchangeCacheKey(req.Code))
	if !ok || token == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid or expired code",
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"data":    fiber.Map{"token": token},
	})
}

// Me retrieves current user information
func (h *AuthHandler) Me(c *fiber.Ctx) error {
	// Get user from context (set by Auth middleware)
	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	ctx := c.Context()
	collection := h.DB.Collections().Users

	// Find user by ID
	var userData models.User
	objID := user.UserID
	err := collection.FindOne(ctx, bson.M{"_id": objID}).Decode(&userData)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "User not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Database error",
			"error":   err.Error(),
		})
	}

	// Return user info
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "User profile retrieved successfully",
		"data": models.UserResponse{
			ID:           userData.ID,
			Name:         userData.Name,
			Email:        userData.Email,
			Role:         userData.Role,
			Picture:      userData.Picture,
			AuthProvider: userData.AuthProvider,
		},
	})
}

// RefreshToken issues a new access token using the refresh token in the cookie
func (h *AuthHandler) RefreshToken(c *fiber.Ctx) error {
	refreshToken := c.Cookies("refresh_token")
	if refreshToken == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "No refresh token provided",
		})
	}

	// Parse and validate the refresh token
	token, err := jwt.Parse(refreshToken, func(token *jwt.Token) (interface{}, error) {
		// Validate the signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(h.Config.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Invalid refresh token",
		})
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || claims["userId"] == nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Invalid token claims",
		})
	}

	// Normalize userId from claims to hex string
	var userIDHex string
	switch v := claims["userId"].(type) {
	case string:
		userIDHex = v
	case primitive.ObjectID:
		userIDHex = v.Hex()
	case fmt.Stringer:
		userIDHex = v.String()
	default:
		userIDHex = fmt.Sprintf("%v", v)
	}

	if userIDHex == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Invalid token userId",
		})
	}

	// Convert to ObjectID for DB lookup
	objID, err := primitive.ObjectIDFromHex(userIDHex)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Invalid user ID format",
		})
	}

	// Optionally, check if user exists in DB
	collection := h.DB.Collections().Users
	var user models.User
	err = collection.FindOne(c.Context(), bson.M{"_id": objID}).Decode(&user)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "User not found",
		})
	}

	// Issue new access token
	accessToken, err := h.generateToken(userIDHex, user.Role)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to generate access token",
		})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"token":   accessToken,
	})
}

// generateToken generates a JWT token
func (h *AuthHandler) generateToken(userID, role string) (string, error) {
	// Create token
	token := jwt.New(jwt.SigningMethodHS256)

	// Set claims
	claims := token.Claims.(jwt.MapClaims)
	claims["userId"] = userID
	claims["role"] = role
	claims["exp"] = time.Now().Add(time.Duration(h.Config.JWTExpirationHours) * time.Hour).Unix()

	// Generate encoded token
	tokenString, err := token.SignedString([]byte(h.Config.JWTSecret))
	if err != nil {
		return "", err
	}

	return tokenString, nil
}

// generateRefreshToken generates a refresh token
func (h *AuthHandler) generateRefreshToken(userID string) (string, error) {
	// Create token
	token := jwt.New(jwt.SigningMethodHS256)

	// Set claims
	claims := token.Claims.(jwt.MapClaims)
	claims["userId"] = userID
	claims["exp"] = time.Now().Add(30 * 24 * time.Hour).Unix() // 30 days

	// Generate encoded token
	tokenString, err := token.SignedString([]byte(h.Config.JWTSecret))
	if err != nil {
		return "", err
	}

	return tokenString, nil
}
