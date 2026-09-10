// Command migrate-uploads pushes every file still sitting in the local
// ./uploads directory into Firebase Storage, then repoints settings.logo at
// the canonical Firebase URL if it is still a legacy "/uploads/<name>"
// reference.
//
// This exists because two things in the codebase assume local-disk uploads
// have already been migrated to Firebase under their exact original object
// name:
//
//   - internal/imageurl.Resolve rewrites any stored "/uploads/<object>"
//     product-image reference to https://storage.googleapis.com/<bucket>/<object>
//     without a per-file mapping -- it assumes an object of that exact name
//     already exists in the bucket.
//   - Once the local ./uploads directory and its Static mount
//     (registerMediaRoutes in internal/handlers/routes_catalog.go) are
//     removed, those files are gone -- and Resolve's rewritten URL is the
//     only remaining way to reach them.
//
// This tool uploads each local file with its filename preserved exactly as
// the object name (unlike FirebaseClient.UploadFile, which mints a fresh
// "<unixnano>-<name>" object name on every call -- that would orphan every
// existing "/uploads/<name>" reference already stored in Mongo). Object
// names in this codebase already carry a unique nanosecond-timestamp prefix
// from their original upload, so collisions are not a concern.
//
// It runs in dry-run mode by default and prints what it would do. Pass
// -apply to write.
//
// Usage:
//
//	go run ./cmd/migrate-uploads                # report only, uploads nothing
//	go run ./cmd/migrate-uploads -apply          # upload files and repoint settings.logo
//	go run ./cmd/migrate-uploads -dir ./uploads -apply
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/firebase"
)

func main() {
	apply := flag.Bool("apply", false, "upload files and update settings.logo; without this flag the tool only reports")
	dir := flag.String("dir", "./uploads", "local directory holding legacy uploaded files")
	flag.Parse()

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	fbClient, err := firebase.NewFirebaseClient(ctx, cfg.FirebaseCredentialsJSON, cfg.FirebaseBucketName)
	if err != nil {
		log.Fatalf("init firebase client: %v", err)
	}

	entries, err := os.ReadDir(*dir)
	if err != nil {
		log.Fatalf("read dir %s: %v", *dir, err)
	}

	if !*apply {
		log.Printf("DRY RUN -- pass -apply to actually upload and rewrite settings.logo")
	}

	migrated := map[string]bool{}
	var uploaded, alreadyPresent, skipped int

	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		name := entry.Name()
		objectPath := filepath.Join(*dir, name)

		exists, err := objectExists(ctx, fbClient, name)
		if err != nil {
			log.Printf("skip %s: check existence: %v", name, err)
			skipped++
			continue
		}
		if exists {
			log.Printf("already in bucket, skipping: %s", name)
			migrated[name] = true
			alreadyPresent++
			continue
		}

		if !*apply {
			fmt.Printf("  would upload %s -> gs://%s/%s\n", objectPath, cfg.FirebaseBucketName, name)
			migrated[name] = true
			continue
		}

		if err := uploadPreservingName(ctx, fbClient, objectPath, name); err != nil {
			log.Printf("skip %s: upload failed: %v", name, err)
			skipped++
			continue
		}
		log.Printf("uploaded: %s", name)
		migrated[name] = true
		uploaded++
	}

	if *apply {
		log.Printf("upload complete: %d uploaded, %d already present, %d skipped", uploaded, alreadyPresent, skipped)
	} else {
		log.Printf("dry run complete: %d would be uploaded, %d already present, %d would be skipped", len(entries)-alreadyPresent-skipped, alreadyPresent, skipped)
	}

	if err := migrateSettingsLogo(ctx, cfg, migrated, *apply); err != nil {
		log.Printf("settings.logo not updated: %v", err)
	}
}

// objectExists reports whether name already exists in the configured bucket.
func objectExists(ctx context.Context, fb *firebase.FirebaseClient, name string) (bool, error) {
	_, err := fb.StorageClient.Bucket(fb.BucketName).Object(name).Attrs(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	return false, err
}

// uploadPreservingName uploads localPath to the bucket under objectName
// verbatim -- no timestamp re-prefixing -- so existing "/uploads/<objectName>"
// references already stored in Mongo keep resolving to the right file.
func uploadPreservingName(ctx context.Context, fb *firebase.FirebaseClient, localPath, objectName string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	obj := fb.StorageClient.Bucket(fb.BucketName).Object(objectName)
	wc := obj.NewWriter(ctx)
	wc.ContentType = contentTypeFor(objectName)

	if _, err := io.Copy(wc, f); err != nil {
		wc.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := obj.ACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		return fmt.Errorf("set public acl: %w", err)
	}
	return nil
}

func contentTypeFor(filename string) string {
	switch filepath.Ext(filename) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// migrateSettingsLogo repoints settings.logo at the canonical Firebase URL if
// it is still a legacy "/uploads/<name>" reference and that name was migrated
// in this run (or a previous one).
func migrateSettingsLogo(ctx context.Context, cfg *config.Config, migrated map[string]bool, apply bool) error {
	client, db, err := config.InitMongoDB(cfg)
	if err != nil {
		return fmt.Errorf("connect mongo: %w", err)
	}
	defer func() {
		if err := client.Disconnect(context.Background()); err != nil {
			log.Printf("disconnect: %v", err)
		}
	}()

	var settings struct {
		Logo string `bson:"logo"`
	}
	err = db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&settings)
	if err != nil {
		return fmt.Errorf("find settings: %w", err)
	}

	const prefix = "/uploads/"
	if !strings.HasPrefix(settings.Logo, prefix) {
		log.Printf("settings.logo is not a legacy reference, nothing to do: %q", settings.Logo)
		return nil
	}

	name := strings.TrimPrefix(settings.Logo, prefix)
	if !migrated[name] {
		return fmt.Errorf("logo object %q was not found locally or in the bucket -- upload it manually before rewriting settings.logo", name)
	}

	canonical := fmt.Sprintf("https://storage.googleapis.com/%s/%s", cfg.FirebaseBucketName, name)
	if !apply {
		fmt.Printf("  would set settings.logo: %s -> %s\n", settings.Logo, canonical)
		return nil
	}

	_, err = db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"logo": canonical, "updated_at": time.Now()}})
	if err != nil {
		return fmt.Errorf("update settings.logo: %w", err)
	}
	log.Printf("settings.logo updated: %s -> %s", settings.Logo, canonical)
	return nil
}
