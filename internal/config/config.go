package config

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// Config holds the application configuration
type Config struct {
	Port               string
	Environment        string
	FrontendURL        string
	MongoURI           string
	DatabaseName       string
	RedisURI           string
	RedisPassword      string
	JWTSecret          string
	JWTExpirationHours int
	RedisDatabase      int
	// Razorpay settings
	RazorpayKey           string
	RazorpaySecret        string
	RazorpayWebhookSecret string
	// Google OAuth settings
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string
	// Firebase settings
	FirebaseCredentialsJSON string
	FirebaseBucketName      string
	// Delhivery settings
	DelhiveryAPIToken       string
	DelhiveryBaseURL        string
	DelhiveryPickupLocation string
	DelhiverySellerName     string
	DelhiverySellerPhone    string
	DelhiverySellerAddress  string
	DelhiverySellerCity     string
	DelhiverySellerState    string
	DelhiverySellerPincode  string
	DelhiveryReturnAddress  string
	DelhiveryReturnCity     string
	DelhiveryReturnState    string
	DelhiveryReturnPincode  string
	DelhiveryReturnPhone    string
	// DelhiveryWebhookSecret authenticates the Delhivery status callback.
	// Delhivery does not sign its webhooks, so this shared secret is the only
	// authentication available; without it the endpoint refuses to mutate
	// anything rather than trusting an anonymous caller.
	DelhiveryWebhookSecret string
	// DelhiveryFlatShippingCharge is what a Delhivery shipment costs the
	// customer. This integration has no carrier rate API, so the figure is
	// operational configuration rather than a carrier quote.
	DelhiveryFlatShippingCharge float64

	// Shiprocket settings. Credentials are read from the environment and are
	// never logged, serialized or returned by any endpoint.
	ShiprocketEmail          string
	ShiprocketPassword       string
	ShiprocketBaseURL        string
	ShiprocketPickupLocation string
	ShiprocketChannelID      string
	ShiprocketWebhookSecret  string

	// RequireShippingSelection refuses a checkout that carries no delivery
	// selection, instead of quietly shipping at zero charge. Off by default so
	// the flag can be turned on once the storefront is deployed with the
	// courier picker; on, it is the guard that stops a stale client from
	// placing unpriced orders.
	RequireShippingSelection bool

	// ShippingProvider selects the primary carrier for new shipments.
	// Operational configuration, deliberately not part of storefront content:
	// switching it must not require touching merchandising data.
	ShippingProvider string
	SellerGSTIN      string

	// Package defaults, centralized so the two carrier adapters cannot drift.
	// These are declared defaults, not measured product data -- the product
	// model carries no weight or dimensions yet.
	PackageDefaultWeightGrams float64
	PackageDefaultLengthCm    float64
	PackageDefaultBreadthCm   float64
	PackageDefaultHeightCm    float64
}

// LoadConfig loads configuration from environment variables
func LoadConfig() (*Config, error) {
	// Load .env file if it exists
	godotenv.Load()

	// Set defaults and override with environment variables if they exist
	cfg := &Config{
		Port:               getEnv("PORT", "8080"),
		Environment:        getEnv("ENVIRONMENT", "development"),
		FrontendURL:        getEnv("FRONTEND_URL", "http://localhost:3000"), // Default to localhost for development
		MongoURI:           getEnv("MONGO_URI", "mongodb://localhost:27017"),
		DatabaseName:       getEnv("DATABASE_NAME", "makwatches"),
		RedisURI:           getEnv("REDIS_URI", "localhost:6379"),
		RedisPassword:      getEnv("REDIS_PASSWORD", ""),
		JWTSecret:          getEnv("JWT_SECRET", "your_jwt_secret_key_here"),
		JWTExpirationHours: getEnvAsInt("JWT_EXPIRATION_HOURS", 24),
		RedisDatabase:      getEnvAsInt("REDIS_DATABASE", 0),
		// Razorpay config (support both KEY/SECRET and KEY_ID/KEY_SECRET naming)
		RazorpayKey: func() string {
			v := getEnv("RAZORPAY_KEY", "")
			if v != "" {
				return v
			}
			return getEnv("RAZORPAY_KEY_ID", "")
		}(),
		RazorpaySecret: func() string {
			v := getEnv("RAZORPAY_SECRET", "")
			if v != "" {
				return v
			}
			return getEnv("RAZORPAY_KEY_SECRET", "")
		}(),
		RazorpayWebhookSecret: getEnv("RAZORPAY_WEBHOOK_SECRET", ""),
		// Google OAuth config
		GoogleClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),
		GoogleRedirectURL:  getEnv("GOOGLE_REDIRECT_URL", "http://localhost:8080/auth/google/callback"),
		// Firebase config
		FirebaseCredentialsJSON: getEnv("FIREBASE_CREDENTIALS_JSON", ""),
		FirebaseBucketName:      getEnv("FIREBASE_BUCKET_NAME", "mak-watches.firebasestorage.app"),
		// Delhivery config
		DelhiveryAPIToken:       getEnv("DELHIVERY_API_TOKEN", ""),
		DelhiveryBaseURL:        getEnv("DELHIVERY_BASE_URL", "https://track.delhivery.com"), // Use https://staging-express.delhivery.com for staging
		DelhiveryPickupLocation: getEnv("DELHIVERY_PICKUP_LOCATION", "Shree Ganesh Watch"),
		DelhiverySellerName:     getEnv("DELHIVERY_SELLER_NAME", "Mak Watches"),
		DelhiverySellerPhone:    getEnv("DELHIVERY_SELLER_PHONE", "9974959693"),
		DelhiverySellerAddress:  getEnv("DELHIVERY_SELLER_ADDRESS", "Shree Ganesh Watch, Matva Street, Near Balaji Complex, Stand chowk, Jetpur, Rajkot"),
		DelhiverySellerCity:     getEnv("DELHIVERY_SELLER_CITY", "Jetpur"),
		DelhiverySellerState:    getEnv("DELHIVERY_SELLER_STATE", "Gujarat"),
		DelhiverySellerPincode:  getEnv("DELHIVERY_SELLER_PINCODE", "360370"),
		DelhiveryReturnAddress:  getEnv("DELHIVERY_RETURN_ADDRESS", "Shree Ganesh Watch, Matva Street, Near Balaji Complex, Stand chowk, Jetpur, Rajkot"),
		DelhiveryReturnCity:     getEnv("DELHIVERY_RETURN_CITY", "Jetpur"),
		DelhiveryReturnState:    getEnv("DELHIVERY_RETURN_STATE", "Gujarat"),
		DelhiveryReturnPincode:  getEnv("DELHIVERY_RETURN_PINCODE", "360370"),
		DelhiveryReturnPhone:    getEnv("DELHIVERY_RETURN_PHONE", "9974959693"),
		DelhiveryWebhookSecret:  getEnv("DELHIVERY_WEBHOOK_SECRET", ""),
		// Zero means shipping is included in the item price, which is what
		// this store does today.
		DelhiveryFlatShippingCharge: getEnvAsFloat("DELHIVERY_FLAT_SHIPPING_CHARGE", 0),

		// Shiprocket config
		ShiprocketEmail:          getEnv("SHIPROCKET_EMAIL", ""),
		ShiprocketPassword:       getEnv("SHIPROCKET_PASSWORD", ""),
		ShiprocketBaseURL:        getEnv("SHIPROCKET_BASE_URL", "https://apiv2.shiprocket.in/v1/external"),
		ShiprocketPickupLocation: getEnv("SHIPROCKET_PICKUP_LOCATION", ""),
		ShiprocketChannelID:      getEnv("SHIPROCKET_CHANNEL_ID", ""),
		ShiprocketWebhookSecret:  getEnv("SHIPROCKET_WEBHOOK_SECRET", ""),

		ShippingProvider:         getEnv("SHIPPING_PROVIDER", "shiprocket"),
		RequireShippingSelection: getEnv("REQUIRE_SHIPPING_SELECTION", "false") == "true",
		SellerGSTIN:              getEnv("SELLER_GSTIN", ""),

		// Carried over from the values the previous integration hardcoded in
		// two places. They are defaults for a wristwatch parcel, not verified
		// measurements, and are recorded as such on every shipment.
		PackageDefaultWeightGrams: getEnvAsFloat("PACKAGE_DEFAULT_WEIGHT_GRAMS", 500),
		PackageDefaultLengthCm:    getEnvAsFloat("PACKAGE_DEFAULT_LENGTH_CM", 15),
		PackageDefaultBreadthCm:   getEnvAsFloat("PACKAGE_DEFAULT_BREADTH_CM", 10),
		PackageDefaultHeightCm:    getEnvAsFloat("PACKAGE_DEFAULT_HEIGHT_CM", 8),
	}

	return cfg, nil
}

// InitMongoDB initializes the MongoDB client
func InitMongoDB(config *Config) (*mongo.Client, *mongo.Database, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Set client options with increased timeout and better error handling
	clientOptions := options.Client().
		ApplyURI(config.MongoURI).
		SetConnectTimeout(5 * time.Second).
		SetServerSelectionTimeout(5 * time.Second)

	log.Printf("Attempting to connect to MongoDB at %s...", RedactURI(config.MongoURI))

	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Printf("MongoDB connection error: %v", err)
		return nil, nil, err
	}

	// Ping the database to verify connection
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()

	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		log.Printf("MongoDB ping failed: %v", err)
		log.Println("Ensure MongoDB is running and the connection URI is correct")
		return nil, nil, err
	}

	log.Println("Connected to MongoDB successfully!")
	db := client.Database(config.DatabaseName)
	return client, db, nil
}

// InitRedis initializes the Redis client
func InitRedis(config *Config) (*redis.Client, error) {
	log.Printf("Attempting to connect to Redis at %s...", config.RedisURI)

	client := redis.NewClient(&redis.Options{
		Addr:        config.RedisURI,
		Password:    config.RedisPassword, // no password by default
		DB:          config.RedisDatabase, // use default DB
		DialTimeout: 5 * time.Second,
		ReadTimeout: 3 * time.Second,
	})

	// Ping the Redis server to verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Ping(ctx).Result()
	if err != nil {
		log.Printf("Redis connection error: %v", err)
		log.Println("Ensure Redis is running and the connection details are correct")

		// If in development mode and Redis is optional, we could return a mock client or nil
		if config.Environment == "development" {
			log.Println("In development mode - continuing without Redis. Caching will be unavailable.")
			return client, nil
		}

		return nil, err
	}

	log.Println("Connected to Redis successfully!")
	return client, nil
}

// getEnv gets the environment variable with fallback
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// getEnvAsInt gets the environment variable as an integer with fallback
func getEnvAsInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		result, err := strconv.Atoi(value)
		if err == nil {
			return result
		}
	}
	return fallback
}

// RedactURI strips the credentials out of a connection string so it can be
// logged.
//
// The startup log used to print MONGO_URI verbatim, which put the database
// password into the application log, the container log and anything shipping
// those onward. The host and database are the useful part for diagnosing a
// connection problem; the credential never is.
func RedactURI(uri string) string {
	scheme := ""
	rest := uri
	if idx := strings.Index(uri, "://"); idx >= 0 {
		scheme = uri[:idx+3]
		rest = uri[idx+3:]
	}
	// Credentials, when present, are everything before the last '@' of the
	// authority section.
	authorityEnd := len(rest)
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		authorityEnd = slash
	}
	authority := rest[:authorityEnd]
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		user := authority[:at]
		if colon := strings.IndexByte(user, ':'); colon >= 0 {
			user = user[:colon]
		}
		return scheme + user + ":***@" + rest[at+1:]
	}
	return uri
}

// getEnvAsFloat gets the environment variable as a float with fallback. An
// unparseable value falls back rather than failing startup, matching how
// getEnvAsInt already behaves.
func getEnvAsFloat(key string, fallback float64) float64 {
	if value, ok := os.LookupEnv(key); ok {
		if result, err := strconv.ParseFloat(value, 64); err == nil {
			return result
		}
		log.Printf("[CONFIG] %s is not a number; using default %v", key, fallback)
	}
	return fallback
}

// GetEnvOrDefault returns the environment variable value or a fallback
func (c *Config) GetEnvOrDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
