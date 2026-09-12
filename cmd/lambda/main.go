package main

import (
	"context"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	fiberadapter "github.com/awslabs/aws-lambda-go-api-proxy/fiber"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/debuglog"
	"github.com/shivam-mishra-20/mak-watches-be/internal/handlers"
)

// Built once per cold start (this init runs once per warm execution
// environment) and reused across every warm invocation that follows -- see
// cmd/api/main.go's equivalent one-time setup, which this mirrors.
var fiberLambda *fiberadapter.FiberLambda

func init() {
	ctx := context.Background()

	// FIREBASE_CREDENTIALS_JSON is not set as a plain Lambda environment
	// variable: at ~2.3KB it alone would eat most of Lambda's 4KB combined
	// env-var budget. deploy/aws/deploy.sh instead stores it in Secrets
	// Manager and sets FIREBASE_SECRET_ARN; fetch it once here and populate
	// the process environment before config.LoadConfig runs, so LoadConfig's
	// existing getEnv("FIREBASE_CREDENTIALS_JSON", "") picks it up exactly as
	// it does today from a real env var. Unset (local dev, cmd/api) -> skip.
	if secretARN := os.Getenv("FIREBASE_SECRET_ARN"); secretARN != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			log.Fatal("Failed to load AWS SDK config for Secrets Manager: ", err)
		}
		out, err := secretsmanager.NewFromConfig(awsCfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId: &secretARN,
		})
		if err != nil {
			log.Fatal("Failed to fetch Firebase credentials from Secrets Manager: ", err)
		}
		if out.SecretString != nil {
			os.Setenv("FIREBASE_CREDENTIALS_JSON", *out.SecretString)
		}
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatal("Failed to load configuration: ", err)
	}
	debuglog.SetEnabled(cfg.Environment != "production")

	mongoClient, _, err := config.InitMongoDB(cfg)
	if err != nil {
		log.Fatal("Cannot continue without database connection: ", err)
	}
	// Deliberately never Disconnect()'d: this init() runs once per cold
	// start and the client must outlive every warm invocation, not just this
	// call -- see internal/database/database.go's DBClient doc comment.

	redisClient, err := config.InitRedis(cfg)
	if err != nil {
		log.Printf("Warning: Redis connection failed: %v", err)
		redisClient = nil
	}

	dbClient := database.NewDBClient(mongoClient, cfg.DatabaseName, redisClient)

	app := fiber.New(fiber.Config{
		AppName:      "Makwatches API",
		ErrorHandler: customErrorHandler,
		BodyLimit:    10 * 1024 * 1024, // 10MB
	})

	prodOrigins := cfg.GetEnvOrDefault("ALLOWED_ORIGINS", "https://makwatches.in,https://www.makwatches.in,https://admin.makwatches.in,http://admin.makwatches.in,https://mak-watches.vercel.app,https://makwatches-admin.vercel.app")
	devOrigins := cfg.GetEnvOrDefault("DEV_ORIGINS", "http://localhost:4200,http://localhost:3000")
	app.Use(cors.New(cors.Config{
		AllowOrigins: prodOrigins + "," + devOrigins,
		AllowOriginsFunc: func(origin string) bool {
			if origin == "" {
				return false
			}
			u, err := url.Parse(origin)
			if err != nil {
				return false
			}
			host := u.Hostname()
			if host == "makwatches.in" || strings.HasSuffix(host, ".makwatches.in") {
				return true
			}
			if host == "localhost" || host == "127.0.0.1" {
				return true
			}
			if strings.HasSuffix(host, ".vercel.app") {
				return true
			}
			return false
		},
		AllowMethods:     "GET,POST,PUT,DELETE,OPTIONS,PATCH",
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization, X-Requested-With, X-CSRF-Token",
		AllowCredentials: true,
		ExposeHeaders:    "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers",
	}))

	handlers.SetupRoutes(app, dbClient, cfg)

	fiberLambda = fiberadapter.New(app)
}

// customErrorHandler provides consistent error responses, mirroring
// cmd/api/main.go's.
func customErrorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
	}
	return c.Status(code).JSON(fiber.Map{
		"success": false,
		"message": "An error occurred",
		"error":   err.Error(),
	})
}

func handleRequest(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	return fiberLambda.ProxyWithContextV2(ctx, req)
}

func main() {
	lambda.Start(handleRequest)
}
