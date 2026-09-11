package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	fbClient, err := firebase.NewFirebaseClient(context.Background(), cfg.FirebaseCredentialsJSON, cfg.FirebaseBucketName)
	if err != nil {
		log.Fatalf("Failed to create Firebase client: %v", err)
	}

	imagePath := "../makwatches-fe/public/welcome-banner.jpg"
	file, err := os.Open(imagePath)
	if err != nil {
		log.Fatalf("Failed to open image file: %v", err)
	}
	defer file.Close()

	url, err := fbClient.UploadFile(context.Background(), file, "welcome-banner.jpg")
	if err != nil {
		log.Fatalf("Failed to upload image: %v", err)
	}

	fmt.Printf("UPLOAD_SUCCESS_URL: %s\n", url)
}
