package handlers

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/debuglog"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// CreateProduct adds a new product to the database (admin only)
func (h *ProductHandler) CreateProduct(c *fiber.Ctx) error {
	ctx := c.Context()

	// Prepare product and helper container for uploaded images
	var product models.Product
	uploadedImages := []string{}

	// Resolve the shared Firebase client so we can upload files if present.
	// Product images always go to Firebase Storage. The previous
	// development-only local fallback persisted request-host URLs
	// (c.BaseURL()+"/uploads/...") into the database, which is what left the
	// catalog pointing at a domain that never served the files.
	fbClient, err := h.Firebase.Client(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to initialize Firebase client",
			"error":   err.Error(),
		})
	}

	// If this is a multipart form, try to read files first so we don't lose the stream when parsing body
	if form, ferr := c.MultipartForm(); ferr == nil {
		// Accept both "images" (multiple) and "image" (single) form keys
		files := form.File["images"]
		if len(files) == 0 {
			files = form.File["image"]
		}
		if len(files) > 0 {
			for _, fh := range files {
				{
					fileReader, err := fh.Open()
					if err != nil {
						return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
							"success": false,
							"message": "Failed to open uploaded file",
							"error":   err.Error(),
						})
					}
					imageURL, err := fbClient.UploadFile(ctx, fileReader, fh.Filename)
					fileReader.Close()
					if err != nil {
						return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
							"success": false,
							"message": "Failed to upload image to Firebase Storage",
							"error":   err.Error(),
						})
					}
					uploadedImages = append(uploadedImages, imageURL)
				}
			}
		}
	}

	// Parse product data (fields). BodyParser works for both JSON and form fields.
	if err := c.BodyParser(&product); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product data",
			"error":   err.Error(),
		})
	}

	// Handle images from multiple sources:
	// Priority 1: If images array was provided in JSON body, use those (pre-uploaded URLs)
	// Priority 2: If files were uploaded in multipart form, merge them with any from JSON

	// If files were uploaded via multipart, merge them with any images from JSON body
	if len(uploadedImages) > 0 {
		if product.Images == nil {
			product.Images = []string{}
		}
		// Merge uploaded files with any images from JSON body
		product.Images = append(product.Images, uploadedImages...)
	}

	// Set ImageURL if not provided
	if product.ImageURL == "" && len(product.Images) > 0 {
		product.ImageURL = product.Images[0]
	}

	// Derive/normalize category pieces (backward compatibility)
	// If Category not provided but MainCategory/Subcategory are, compose Category.
	if product.Category == "" && product.MainCategory != "" {
		if product.Subcategory != "" {
			product.Category = product.MainCategory + "/" + product.Subcategory
		} else {
			product.Category = product.MainCategory
		}
	}

	// Validate required fields (Name, Description, Price, Category)
	if product.Name == "" || product.Description == "" || product.Price <= 0 || product.Category == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Missing required product fields",
		})
	}

	// (image uploads already handled above)

	// Derive MainCategory/Subcategory from Category if not individually provided
	if product.MainCategory == "" && product.Category != "" {
		parts := strings.Split(product.Category, "/")
		if len(parts) > 0 {
			product.MainCategory = parts[0]
		}
		if len(parts) > 1 {
			product.Subcategory = parts[1]
		}
	}

	// Set timestamps
	product.CreatedAt = time.Now()
	product.UpdatedAt = product.CreatedAt

	// Insert product into database (ensure we store brand/mainCategory/subcategory fields as well)
	collection := h.DB.Collections().Products
	result, err := collection.InsertOne(ctx, product)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to create product",
			"error":   err.Error(),
		})
	}

	// Get the inserted ID
	product.ID = result.InsertedID.(primitive.ObjectID)

	// Invalidate the cached product listings. This used to CacheDel a key
	// ("products:"+category) that never matched the real cached key format
	// (products:v<n>:<category>:<minPrice>:<maxPrice>:<sortBy>:<order>:<page>:
	// <limit> -- see GetProducts), so it silently did nothing. Bumping the
	// version invalidates every cached listing page/filter/sort at once.
	h.DB.BumpCacheVersion(ctx, "products")

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success": true,
		"message": "Product created successfully",
		"data":    product,
	})
}

// UpdateProduct updates an existing product (admin only)
func (h *ProductHandler) UpdateProduct(c *fiber.Ctx) error {
	debuglog.Printf("[UpdateProduct] Called for ID: %s\n", c.Params("id"))
	debuglog.Printf("[UpdateProduct] Incoming body: %s\n", string(c.BodyRaw()))

	ctx := c.Context()

	// Get product ID
	id := c.Params("id")
	if id == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Product ID is required",
		})
	}

	// Convert string ID to ObjectID
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product ID format",
			"error":   err.Error(),
		})
	}

	// First, get the existing product to check if it exists
	collection := h.DB.Collections().Products
	var existingProduct models.Product
	err = collection.FindOne(ctx, bson.M{"_id": objectID}).Decode(&existingProduct)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"success": false,
				"message": "Product not found",
			})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve product",
			"error":   err.Error(),
		})
	}

	// Fiber handles multipart form parsing automatically

	// Prepare updatedProduct and capture uploaded images first
	var updatedProduct models.Product
	uploadedImages := []string{}

	// Resolve the shared Firebase client for uploads.
	// Product images always go to Firebase Storage. The previous
	// development-only local fallback persisted request-host URLs
	// (c.BaseURL()+"/uploads/...") into the database, which is what left the
	// catalog pointing at a domain that never served the files.
	fbClient, err := h.Firebase.Client(ctx)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to initialize Firebase client",
			"error":   err.Error(),
		})
	}

	// Parse multipart uploads first so body parsing can still work
	if form, ferr := c.MultipartForm(); ferr == nil {
		files := form.File["images"]
		if len(files) == 0 {
			files = form.File["image"]
		}
		if len(files) > 0 {
			for _, fh := range files {
				{
					fileReader, err := fh.Open()
					if err != nil {
						return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
							"success": false,
							"message": "Failed to open uploaded file",
							"error":   err.Error(),
						})
					}
					imageURL, err := fbClient.UploadFile(ctx, fileReader, fh.Filename)
					fileReader.Close()
					if err != nil {
						return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
							"success": false,
							"message": "Failed to upload image to Firebase Storage",
							"error":   err.Error(),
						})
					}
					uploadedImages = append(uploadedImages, imageURL)
				}
			}
		}
	}

	// Parse product data from body (works with form fields or JSON)
	if err := c.BodyParser(&updatedProduct); err != nil {
		debuglog.Printf("[UpdateProduct] Error parsing body: %v\n", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product data",
			"error":   err.Error(),
		})
	}

	// Capture images from JSON body (if provided) before we potentially overwrite them
	imagesFromBody := updatedProduct.Images
	imageUrlFromBody := updatedProduct.ImageURL

	// Keep existing fields if not provided
	if updatedProduct.Name == "" {
		updatedProduct.Name = existingProduct.Name
	}
	if updatedProduct.Description == "" {
		updatedProduct.Description = existingProduct.Description
	}
	if updatedProduct.Brand == "" {
		updatedProduct.Brand = existingProduct.Brand
	}
	if updatedProduct.Price <= 0 {
		updatedProduct.Price = existingProduct.Price
	}
	if updatedProduct.Category == "" {
		updatedProduct.Category = existingProduct.Category
	}
	if updatedProduct.MainCategory == "" {
		updatedProduct.MainCategory = existingProduct.MainCategory
	}
	if updatedProduct.Subcategory == "" {
		updatedProduct.Subcategory = existingProduct.Subcategory
	}
	if updatedProduct.Stock < 0 {
		updatedProduct.Stock = existingProduct.Stock
	}

	// Derive Category if still blank but we have MainCategory/Subcategory
	if updatedProduct.Category == "" && updatedProduct.MainCategory != "" {
		if updatedProduct.Subcategory != "" {
			updatedProduct.Category = updatedProduct.MainCategory + "/" + updatedProduct.Subcategory
		} else {
			updatedProduct.Category = updatedProduct.MainCategory
		}
	}

	// Handle image updates with multiple strategies:
	// Priority 1: If images were provided in JSON body, use those
	// Priority 2: If files were uploaded in multipart form, use those
	// Priority 3: Check keepExistingImages flag to determine whether to keep existing images

	// Check if images were provided in the JSON body
	if len(imagesFromBody) > 0 {
		// Images provided in JSON body (e.g., pre-uploaded URLs)
		updatedProduct.Images = imagesFromBody
		if imageUrlFromBody != "" {
			updatedProduct.ImageURL = imageUrlFromBody
		} else if len(imagesFromBody) > 0 {
			updatedProduct.ImageURL = imagesFromBody[0]
		}
	} else if len(uploadedImages) > 0 {
		// Files were uploaded in this request
		// Handle keepExistingImages flag for multipart uploads
		keepExistingImages := true
		if keepImagesStr := c.FormValue("keepExistingImages"); keepImagesStr != "" {
			keepExistingImages, _ = strconv.ParseBool(keepImagesStr)
		}

		if keepExistingImages {
			// Merge with existing images
			updatedProduct.Images = append(existingProduct.Images, uploadedImages...)
			if updatedProduct.ImageURL == "" {
				updatedProduct.ImageURL = existingProduct.ImageURL
			}
		} else {
			// Replace with new images
			updatedProduct.Images = uploadedImages
			updatedProduct.ImageURL = uploadedImages[0]
		}
	} else {
		// No new images provided, keep existing
		updatedProduct.Images = existingProduct.Images
		updatedProduct.ImageURL = existingProduct.ImageURL
	}

	// Note: uploads were already processed earlier into uploadedImages and merged above.

	// Ensure at least one image if neither images nor imageUrl were provided
	if len(updatedProduct.Images) == 0 && updatedProduct.ImageURL == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Product must have at least one image",
		})
	}

	// Keep original ID and created timestamp
	updatedProduct.ID = objectID
	updatedProduct.CreatedAt = existingProduct.CreatedAt
	updatedProduct.UpdatedAt = time.Now()

	// Update the product in database (including new brand/main/subcategory fields)
	update := bson.M{
		"$set": bson.M{
			"name":          updatedProduct.Name,
			"description":   updatedProduct.Description,
			"brand":         updatedProduct.Brand,
			"price":         updatedProduct.Price,
			"category":      updatedProduct.Category,
			"main_category": updatedProduct.MainCategory,
			"subcategory":   updatedProduct.Subcategory,
			"image_url":     updatedProduct.ImageURL,
			"images":        updatedProduct.Images,
			"stock":         updatedProduct.Stock,
			// filterable attributes
			"gender":         updatedProduct.Gender,
			"dial_color":     updatedProduct.DialColor,
			"dial_shape":     updatedProduct.DialShape,
			"dial_type":      updatedProduct.DialType,
			"strap_color":    updatedProduct.StrapColor,
			"strap_material": updatedProduct.StrapMaterial,
			"style":          updatedProduct.Style,
			"dial_thickness": updatedProduct.DialThickness,
			// Discount fields (optional)
			"discount_percentage": updatedProduct.DiscountPercentage,
			"discount_amount":     updatedProduct.DiscountAmount,
			"discount_start_date": updatedProduct.DiscountStartDate,
			"discount_end_date":   updatedProduct.DiscountEndDate,
			"updated_at":          updatedProduct.UpdatedAt,
		},
	}

	_, err = collection.UpdateOne(ctx, bson.M{"_id": objectID}, update)
	if err != nil {
		debuglog.Printf("[UpdateProduct] Error updating product: %v\n", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to update product",
			"error":   err.Error(),
		})
	}

	// Invalidate cache: the single-product key is exact and still correctly
	// targeted; the product-listing cache (see BumpCacheVersion's caller in
	// CreateProduct for why) can't be targeted by key, so bump its version
	// instead -- covers both the old and new category in one call.
	cacheKey := fmt.Sprintf("product:%s", id)
	h.DB.CacheDel(ctx, cacheKey)
	h.DB.BumpCacheVersion(ctx, "products")

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Product updated successfully",
		"data":    updatedProduct,
	})
}

// DeleteProduct removes a product from the database (admin only)
func (h *ProductHandler) DeleteProduct(c *fiber.Ctx) error {
	debuglog.Printf("[DeleteProduct] Called for ID: %s\n", c.Params("id"))
	defer func() {
		debuglog.Printf("[DeleteProduct] Completed for ID: %s\n", c.Params("id"))
	}()

	ctx := c.Context()

	// Get product ID
	id := c.Params("id")
	if id == "" {
		debuglog.Printf("[DeleteProduct] Product ID missing\n")
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Product ID is required",
		})
	}

	// Convert string ID to ObjectID
	objectID, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		debuglog.Printf("[DeleteProduct] Invalid product ID format: %v\n", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product ID format",
			"error":   err.Error(),
		})
	}

	// First, get the product to delete (to get image URLs)
	collection := h.DB.Collections().Products
	var product models.Product
	// Find but don't error if not found
	findErr := collection.FindOne(ctx, bson.M{"_id": objectID}).Decode(&product)

	// Delete the product
	deleteResult, err := collection.DeleteOne(ctx, bson.M{"_id": objectID})
	if err != nil {
		debuglog.Printf("[DeleteProduct] Error deleting product: %v\n", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to delete product",
			"error":   err.Error(),
		})
	}
	if deleteResult.DeletedCount == 0 {
		debuglog.Printf("[DeleteProduct] No product deleted for ID: %s\n", id)
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Product not found or already deleted",
		})
	}

	// After finding the product
	if findErr != nil {
		debuglog.Printf("[DeleteProduct] Error finding product: %v\n", findErr)
	} else {
		debuglog.Printf("[DeleteProduct] Found product: %+v\n", product)
	}

	// After deleting
	debuglog.Printf("[DeleteProduct] Delete result: %+v\n", deleteResult)

	// Before deleting images
	if findErr == nil && len(product.Images) > 0 {
		debuglog.Printf("[DeleteProduct] Deleting images: %+v\n", product.Images)
	}

	// ALWAYS delete images if product existed and had images
	if findErr == nil && len(product.Images) > 0 {
		// Note: Firebase Storage files are automatically managed and don't need manual deletion
		// The files will remain in Firebase Storage but are no longer referenced in the database

		// 2. Also remove local files if they exist in uploads directory
		// This handles both local-only and S3+local scenarios
		for _, imageURL := range product.Images {
			// Extract filename from URL path (e.g., http://localhost:8080/uploads/1234-image.jpg -> 1234-image.jpg)
			parts := strings.Split(imageURL, "/")
			if len(parts) > 0 {
				filename := parts[len(parts)-1]
				// Delete from local filesystem
				localPath := fmt.Sprintf("uploads/%s", filename)
				os.Remove(localPath) // Ignore errors, best effort
			}
		}
	}

	// Invalidate cache
	debuglog.Printf("[DeleteProduct] Invalidating cache for product:%s\n", id)
	h.DB.CacheDel(ctx, fmt.Sprintf("product:%s", id))

	// Invalidate the product-listing cache (see BumpCacheVersion's caller in
	// CreateProduct for why a direct CacheDel can't target it).
	debuglog.Printf("[DeleteProduct] Invalidating products listing cache\n")
	h.DB.BumpCacheVersion(ctx, "products")

	debuglog.Printf("[DeleteProduct] Product deleted successfully for ID: %s\n", id)
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Product and associated resources deleted successfully",
		"data": fiber.Map{
			"deleted": deleteResult.DeletedCount > 0,
		},
	})
}
