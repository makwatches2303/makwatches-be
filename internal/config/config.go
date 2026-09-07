package config

import (
	"context"
	"log"
	"os"
	"strconv"
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

	log.Printf("Attempting to connect to MongoDB at %s...", config.MongoURI)

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

// GetEnvOrDefault returns the environment variable value or a fallback
func (c *Config) GetEnvOrDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
