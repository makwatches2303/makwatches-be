package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type HeroSlide struct {
	ID                 primitive.ObjectID  `bson:"_id,omitempty"`
	ProductID          *primitive.ObjectID `bson:"productId,omitempty"`
	Title              string              `bson:"title"`
	Brand              string              `bson:"brand,omitempty"`
	Description        string              `bson:"description"`
	ProductPrice       float64             `bson:"productPrice,omitempty"`
	Category           string              `bson:"category,omitempty"`
	MainCategory       string              `bson:"mainCategory,omitempty"`
	Subcategory        string              `bson:"subcategory,omitempty"`
	Image              string              `bson:"image"`
	Images             []string            `bson:"images,omitempty"`
	Stock              int                 `bson:"stock,omitempty"`
	Gender             string              `bson:"gender,omitempty"`
	DialColor          string              `bson:"dialColor,omitempty"`
	DialShape          string              `bson:"dialShape,omitempty"`
	DialType           string              `bson:"dialType,omitempty"`
	StrapColor         string              `bson:"strapColor,omitempty"`
	StrapMaterial      string              `bson:"strapMaterial,omitempty"`
	Style              string              `bson:"style,omitempty"`
	DialThickness      string              `bson:"dialThickness,omitempty"`
	DiscountPercentage float64             `bson:"discountPercentage,omitempty"`
	DiscountAmount     float64             `bson:"discountAmount,omitempty"`
	DiscountStartDate  *time.Time          `bson:"discountStartDate,omitempty"`
	DiscountEndDate    *time.Time          `bson:"discountEndDate,omitempty"`
}

type CollectionFeature struct {
	ID                 primitive.ObjectID  `bson:"_id,omitempty"`
	ProductID          *primitive.ObjectID `bson:"productId,omitempty"`
	Title              string              `bson:"title"`
	Brand              string              `bson:"brand,omitempty"`
	Description        string              `bson:"description"`
	ProductPrice       float64             `bson:"productPrice,omitempty"`
	Category           string              `bson:"category,omitempty"`
	MainCategory       string              `bson:"mainCategory,omitempty"`
	Subcategory        string              `bson:"subcategory,omitempty"`
	Image              string              `bson:"image"`
	Images             []string            `bson:"images,omitempty"`
	Stock              int                 `bson:"stock,omitempty"`
	Gender             string              `bson:"gender,omitempty"`
	DialColor          string              `bson:"dialColor,omitempty"`
	DialShape          string              `bson:"dialShape,omitempty"`
	DialType           string              `bson:"dialType,omitempty"`
	StrapColor         string              `bson:"strapColor,omitempty"`
	StrapMaterial      string              `bson:"strapMaterial,omitempty"`
	Style              string              `bson:"style,omitempty"`
	DialThickness      string              `bson:"dialThickness,omitempty"`
	DiscountPercentage float64             `bson:"discountPercentage,omitempty"`
	DiscountAmount     float64             `bson:"discountAmount,omitempty"`
	DiscountStartDate  *time.Time          `bson:"discountStartDate,omitempty"`
	DiscountEndDate    *time.Time          `bson:"discountEndDate,omitempty"`
}

type Product struct {
	ID                 primitive.ObjectID `bson:"_id,omitempty"`
	Name               string             `bson:"name"`
	Brand              string             `bson:"brand,omitempty"`
	Description        string             `bson:"description"`
	Price              float64            `bson:"price"`
	Category           string             `bson:"category,omitempty"`
	MainCategory       string             `bson:"main_category,omitempty"`
	Subcategory        string             `bson:"subcategory,omitempty"`
	ImageURL           string             `bson:"image_url"`
	Images             []string           `bson:"images,omitempty"`
	Stock              int                `bson:"stock"`
	Gender             string             `bson:"gender,omitempty"`
	DialColor          string             `bson:"dial_color,omitempty"`
	DialShape          string             `bson:"dial_shape,omitempty"`
	DialType           string             `bson:"dial_type,omitempty"`
	StrapColor         string             `bson:"strap_color,omitempty"`
	StrapMaterial      string             `bson:"strap_material,omitempty"`
	Style              string             `bson:"style,omitempty"`
	DialThickness      string             `bson:"dial_thickness,omitempty"`
	DiscountPercentage float64            `bson:"discount_percentage,omitempty"`
	DiscountAmount     float64            `bson:"discount_amount,omitempty"`
	DiscountStartDate  *time.Time         `bson:"discount_start_date,omitempty"`
	DiscountEndDate    *time.Time         `bson:"discount_end_date,omitempty"`
	CreatedAt          time.Time          `bson:"created_at"`
	UpdatedAt          time.Time          `bson:"updated_at"`
}

func main() {
	// Get MongoDB URI from environment. No hardcoded fallback: this file used
	// to carry a live Atlas connection string as a default, which meant the
	// credential sat in git history in plaintext. Fail loudly instead.
	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		log.Fatal("MONGODB_URI is required and was not set")
	}

	// Connect to MongoDB
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer client.Disconnect(ctx)

	db := client.Database("makwatches")
	heroCollection := db.Collection("hero_slides")
	collectionFeaturesCollection := db.Collection("home_collection_features")
	productsCollection := db.Collection("products")

	fmt.Println("Starting migration of home content items to products collection...")

	// Migrate Hero Slides
	fmt.Println("\n=== Migrating Hero Slides ===")
	cursor, err := heroCollection.Find(ctx, bson.M{})
	if err != nil {
		log.Fatalf("Failed to fetch hero slides: %v", err)
	}
	defer cursor.Close(ctx)

	heroCount := 0
	heroCreated := 0
	heroUpdated := 0

	for cursor.Next(ctx) {
		var slide HeroSlide
		if err := cursor.Decode(&slide); err != nil {
			log.Printf("Error decoding hero slide: %v", err)
			continue
		}
		heroCount++

		// Determine product ID
		var productID primitive.ObjectID
		if slide.ProductID != nil && !slide.ProductID.IsZero() {
			productID = *slide.ProductID
		} else {
			// If no productId, create a new one and update the hero slide
			productID = primitive.NewObjectID()
			_, err := heroCollection.UpdateByID(ctx, slide.ID, bson.M{
				"$set": bson.M{"productId": productID},
			})
			if err != nil {
				log.Printf("Error updating hero slide %s with productId: %v", slide.ID.Hex(), err)
			}
		}

		// Check if product exists
		var existingProduct Product
		err = productsCollection.FindOne(ctx, bson.M{"_id": productID}).Decode(&existingProduct)

		now := time.Now().UTC()

		if err == mongo.ErrNoDocuments {
			// Create new product with defaults for missing fields
			product := Product{
				ID:                 productID,
				Name:               slide.Title,
				Brand:              slide.Brand,
				Description:        slide.Description,
				Price:              slide.ProductPrice,
				Category:           slide.Category,
				MainCategory:       slide.MainCategory,
				Subcategory:        slide.Subcategory,
				ImageURL:           slide.Image,
				Images:             slide.Images,
				Stock:              slide.Stock,
				Gender:             slide.Gender,
				DialColor:          slide.DialColor,
				DialShape:          slide.DialShape,
				DialType:           slide.DialType,
				StrapColor:         slide.StrapColor,
				StrapMaterial:      slide.StrapMaterial,
				Style:              slide.Style,
				DialThickness:      slide.DialThickness,
				DiscountPercentage: slide.DiscountPercentage,
				DiscountAmount:     slide.DiscountAmount,
				DiscountStartDate:  slide.DiscountStartDate,
				DiscountEndDate:    slide.DiscountEndDate,
				CreatedAt:          now,
				UpdatedAt:          now,
			}

			// Set defaults for missing fields
			if product.Stock == 0 {
				product.Stock = 100
			}
			if product.Price == 0 {
				product.Price = 3800 // Default price
			}
			if product.Brand == "" {
				product.Brand = "MAK"
			}
			if product.Category == "" {
				product.Category = "Watches"
			}
			if product.MainCategory == "" {
				product.MainCategory = "MEN"
			}
			if product.Subcategory == "" {
				product.Subcategory = "LUXURY"
			}
			if product.Gender == "" {
				product.Gender = "Men"
			}
			if product.DialColor == "" {
				product.DialColor = "Black"
			}
			if product.DialShape == "" {
				product.DialShape = "Round"
			}
			if product.DialType == "" {
				product.DialType = "Analog"
			}
			if product.StrapColor == "" {
				product.StrapColor = "Black"
			}
			if product.StrapMaterial == "" {
				product.StrapMaterial = "Metal"
			}
			if product.Style == "" {
				product.Style = "Casual"
			}
			if product.DialThickness == "" {
				product.DialThickness = "Slim"
			}
			if product.Images == nil || len(product.Images) == 0 {
				product.Images = []string{product.ImageURL}
			}

			_, err = productsCollection.InsertOne(ctx, product)
			if err != nil {
				log.Printf("Error creating product for hero slide %s: %v", slide.ID.Hex(), err)
			} else {
				fmt.Printf("✓ Created product %s for hero slide: %s\n", productID.Hex(), slide.Title)
				heroCreated++
			}
		} else if err == nil {
			// Update existing product with defaults for missing fields
			update := bson.M{
				"name":        slide.Title,
				"description": slide.Description,
				"image_url":   slide.Image,
				"updated_at":  now,
			}

			// Only update non-empty fields
			if slide.Brand != "" {
				update["brand"] = slide.Brand
			} else {
				update["brand"] = "MAK"
			}

			if slide.ProductPrice > 0 {
				update["price"] = slide.ProductPrice
			} else {
				update["price"] = 3800.0
			}

			if slide.Category != "" {
				update["category"] = slide.Category
			} else {
				update["category"] = "Watches"
			}

			if slide.MainCategory != "" {
				update["main_category"] = slide.MainCategory
			} else {
				update["main_category"] = "MEN"
			}

			if slide.Subcategory != "" {
				update["subcategory"] = slide.Subcategory
			} else {
				update["subcategory"] = "LUXURY"
			}

			if slide.Gender != "" {
				update["gender"] = slide.Gender
			} else {
				update["gender"] = "Men"
			}

			if slide.DialColor != "" {
				update["dial_color"] = slide.DialColor
			} else {
				update["dial_color"] = "Black"
			}

			if slide.DialShape != "" {
				update["dial_shape"] = slide.DialShape
			} else {
				update["dial_shape"] = "Round"
			}

			if slide.DialType != "" {
				update["dial_type"] = slide.DialType
			} else {
				update["dial_type"] = "Analog"
			}

			if slide.StrapColor != "" {
				update["strap_color"] = slide.StrapColor
			} else {
				update["strap_color"] = "Black"
			}

			if slide.StrapMaterial != "" {
				update["strap_material"] = slide.StrapMaterial
			} else {
				update["strap_material"] = "Metal"
			}

			if slide.Style != "" {
				update["style"] = slide.Style
			} else {
				update["style"] = "Casual"
			}

			if slide.DialThickness != "" {
				update["dial_thickness"] = slide.DialThickness
			} else {
				update["dial_thickness"] = "Slim"
			}

			if slide.Images != nil && len(slide.Images) > 0 {
				update["images"] = slide.Images
			} else if slide.Image != "" {
				update["images"] = []string{slide.Image}
			}

			if slide.Stock > 0 {
				update["stock"] = slide.Stock
			} else {
				update["stock"] = 100
			}

			if slide.DiscountPercentage > 0 {
				update["discount_percentage"] = slide.DiscountPercentage
			}

			if slide.DiscountAmount > 0 {
				update["discount_amount"] = slide.DiscountAmount
			}

			if slide.DiscountStartDate != nil {
				update["discount_start_date"] = slide.DiscountStartDate
			}

			if slide.DiscountEndDate != nil {
				update["discount_end_date"] = slide.DiscountEndDate
			}

			_, err = productsCollection.UpdateByID(ctx, productID, bson.M{"$set": update})
			if err != nil {
				log.Printf("Error updating product %s: %v", productID.Hex(), err)
			} else {
				fmt.Printf("✓ Updated product %s for hero slide: %s\n", productID.Hex(), slide.Title)
				heroUpdated++
			}
		}
	}

	// Migrate Collection Features
	fmt.Println("\n=== Migrating Collection Features ===")
	cursor, err = collectionFeaturesCollection.Find(ctx, bson.M{})
	if err != nil {
		log.Fatalf("Failed to fetch collection features: %v", err)
	}
	defer cursor.Close(ctx)

	collectionCount := 0
	collectionCreated := 0
	collectionUpdated := 0

	for cursor.Next(ctx) {
		var feature CollectionFeature
		if err := cursor.Decode(&feature); err != nil {
			log.Printf("Error decoding collection feature: %v", err)
			continue
		}
		collectionCount++

		// Determine product ID
		var productID primitive.ObjectID
		if feature.ProductID != nil && !feature.ProductID.IsZero() {
			productID = *feature.ProductID
		} else {
			// If no productId, create a new one and update the collection feature
			productID = primitive.NewObjectID()
			_, err := collectionFeaturesCollection.UpdateByID(ctx, feature.ID, bson.M{
				"$set": bson.M{"productId": productID},
			})
			if err != nil {
				log.Printf("Error updating collection feature %s with productId: %v", feature.ID.Hex(), err)
			}
		}

		// Check if product exists
		var existingProduct Product
		err = productsCollection.FindOne(ctx, bson.M{"_id": productID}).Decode(&existingProduct)

		now := time.Now().UTC()

		if err == mongo.ErrNoDocuments {
			// Create new product
			product := Product{
				ID:                 productID,
				Name:               feature.Title,
				Brand:              feature.Brand,
				Description:        feature.Description,
				Price:              feature.ProductPrice,
				Category:           feature.Category,
				MainCategory:       feature.MainCategory,
				Subcategory:        feature.Subcategory,
				ImageURL:           feature.Image,
				Images:             feature.Images,
				Stock:              feature.Stock,
				Gender:             feature.Gender,
				DialColor:          feature.DialColor,
				DialShape:          feature.DialShape,
				DialType:           feature.DialType,
				StrapColor:         feature.StrapColor,
				StrapMaterial:      feature.StrapMaterial,
				Style:              feature.Style,
				DialThickness:      feature.DialThickness,
				DiscountPercentage: feature.DiscountPercentage,
				DiscountAmount:     feature.DiscountAmount,
				DiscountStartDate:  feature.DiscountStartDate,
				DiscountEndDate:    feature.DiscountEndDate,
				CreatedAt:          now,
				UpdatedAt:          now,
			}

			if product.Stock == 0 {
				product.Stock = 100 // Default stock
			}

			_, err = productsCollection.InsertOne(ctx, product)
			if err != nil {
				log.Printf("Error creating product for collection feature %s: %v", feature.ID.Hex(), err)
			} else {
				fmt.Printf("✓ Created product %s for collection feature: %s\n", productID.Hex(), feature.Title)
				collectionCreated++
			}
		} else if err == nil {
			// Update existing product
			update := bson.M{
				"name":                feature.Title,
				"brand":               feature.Brand,
				"description":         feature.Description,
				"price":               feature.ProductPrice,
				"category":            feature.Category,
				"main_category":       feature.MainCategory,
				"subcategory":         feature.Subcategory,
				"image_url":           feature.Image,
				"images":              feature.Images,
				"gender":              feature.Gender,
				"dial_color":          feature.DialColor,
				"dial_shape":          feature.DialShape,
				"dial_type":           feature.DialType,
				"strap_color":         feature.StrapColor,
				"strap_material":      feature.StrapMaterial,
				"style":               feature.Style,
				"dial_thickness":      feature.DialThickness,
				"discount_percentage": feature.DiscountPercentage,
				"discount_amount":     feature.DiscountAmount,
				"discount_start_date": feature.DiscountStartDate,
				"discount_end_date":   feature.DiscountEndDate,
				"updated_at":          now,
			}

			if feature.Stock > 0 {
				update["stock"] = feature.Stock
			}

			_, err = productsCollection.UpdateByID(ctx, productID, bson.M{"$set": update})
			if err != nil {
				log.Printf("Error updating product %s: %v", productID.Hex(), err)
			} else {
				fmt.Printf("✓ Updated product %s for collection feature: %s\n", productID.Hex(), feature.Title)
				collectionUpdated++
			}
		}
	}

	// Summary
	fmt.Println("\n=== Migration Summary ===")
	fmt.Printf("Hero Slides: %d total, %d created, %d updated\n", heroCount, heroCreated, heroUpdated)
	fmt.Printf("Collection Features: %d total, %d created, %d updated\n", collectionCount, collectionCreated, collectionUpdated)
	fmt.Printf("\nTotal products created: %d\n", heroCreated+collectionCreated)
	fmt.Printf("Total products updated: %d\n", heroUpdated+collectionUpdated)
	fmt.Println("\n✓ Migration completed successfully!")
}
