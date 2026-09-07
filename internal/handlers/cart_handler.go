package handlers

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/shivam-mishra-20/mak-watches-be/internal/config"
	"github.com/shivam-mishra-20/mak-watches-be/internal/database"
	"github.com/shivam-mishra-20/mak-watches-be/internal/middleware"
	"github.com/shivam-mishra-20/mak-watches-be/internal/models"
)

// CartHandler handles cart related requests
type CartHandler struct {
	DB     *database.DBClient
	Config *config.Config
}

// NewCartHandler creates a new instance of CartHandler
func NewCartHandler(db *database.DBClient, cfg *config.Config) *CartHandler {
	return &CartHandler{
		DB:     db,
		Config: cfg,
	}
}

// AddToCart adds a product to the user's cart
func (h *CartHandler) AddToCart(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get user info from the token
	userLocals := c.Locals("user")
	if userLocals == nil {
		fmt.Printf("[CART] AddToCart - user locals is nil, Path: %s, Method: %s, IP: %s\n",
			c.Path(), c.Method(), c.IP())
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found in context",
		})
	}

	user, ok := userLocals.(*middleware.TokenMetadata)
	if !ok || user == nil {
		fmt.Printf("[CART] AddToCart - user type assertion failed or user is nil, Path: %s\n", c.Path())
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - Invalid user data format",
		})
	}

	fmt.Printf("[CART] AddToCart - User authenticated: %s\n", user.UserID.Hex())

	// Parse request body
	var req models.CartItemRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// Validate required fields
	if req.ProductID == "" || req.Quantity <= 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Product ID and quantity > 0 are required",
		})
	}

	// Convert product ID from string to ObjectID
	productID, err := primitive.ObjectIDFromHex(req.ProductID)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product ID format",
			"error":   err.Error(),
		})
	}

	// First, try to find in home content (hero slides and collections) by productId
	heroCollection := h.DB.MongoDB.Collection("hero_slides")
	collectionCollection := h.DB.MongoDB.Collection("home_collection_features")

	var heroSlide models.HeroSlide
	err = heroCollection.FindOne(ctx, bson.M{"productId": productID}).Decode(&heroSlide)
	if err == nil {
		// Found in hero slides - home content products don't have stock limits
		fmt.Printf("[CART] Product found in hero_slides: %s (productId: %s)\n", heroSlide.Title, productID.Hex())
		// Product verification successful, continue to add to cart
	} else {
		// Try collection features
		var collectionFeature models.HomeCollectionFeature
		err = collectionCollection.FindOne(ctx, bson.M{"productId": productID}).Decode(&collectionFeature)
		if err == nil {
			fmt.Printf("[CART] Product found in home_collection_features: %s (productId: %s)\n", collectionFeature.Title, productID.Hex())
			// Product verification successful, continue to add to cart
		} else {
			// Not found in home content, try regular products collection by _id
			var product models.Product
			collection := h.DB.Collections().Products
			err = collection.FindOne(ctx, bson.M{"_id": productID}).Decode(&product)
			if err == mongo.ErrNoDocuments {
				// Product not found in any collection
				fmt.Printf("[CART] Product not found anywhere: %s\n", productID.Hex())
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
					"success": false,
					"message": "Product not found",
				})
			} else if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"success": false,
					"message": "Failed to retrieve product",
					"error":   err.Error(),
				})
			} else {
				// Product found in regular products collection - check stock
				if product.Stock < req.Quantity {
					return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
						"success": false,
						"message": "Not enough stock available",
					})
				}
			}
		}
	}

	// Check if the product (same size) is already in the cart. Size empty matches only empty.
	cartCollection := h.DB.Collections().CartItems
	var existingCartItem models.CartItem
	query := bson.M{"user_id": user.UserID, "product_id": productID}
	if req.Size != "" {
		query["size"] = req.Size
	} else {
		query["size"] = bson.M{"$in": bson.A{"", nil}}
	}
	err = cartCollection.FindOne(ctx, query).Decode(&existingCartItem)

	now := time.Now()

	switch err {
	case nil:
		// Update existing cart item
		_, err = cartCollection.UpdateOne(
			ctx,
			bson.M{"_id": existingCartItem.ID},
			bson.M{
				"$set": bson.M{
					"quantity":   existingCartItem.Quantity + req.Quantity,
					"updated_at": now,
				},
			},
		)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to update cart item",
				"error":   err.Error(),
			})
		}
	case mongo.ErrNoDocuments:
		// Add new cart item
		cartItem := models.CartItem{
			ID:        primitive.NewObjectID(),
			UserID:    user.UserID,
			ProductID: productID,
			Size:      req.Size,
			Quantity:  req.Quantity,
			CreatedAt: now,
			UpdatedAt: now,
		}

		_, err = cartCollection.InsertOne(ctx, cartItem)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to add product to cart",
				"error":   err.Error(),
			})
		}
	default:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Database error",
			"error":   err.Error(),
		})
	}

	// Invalidate cart cache
	cacheKey := fmt.Sprintf("cart:%s", user.UserID.Hex())
	h.DB.CacheDel(ctx, cacheKey)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Product added to cart successfully",
	})
}

// GetCart retrieves a user's cart
func (h *CartHandler) GetCart(c *fiber.Ctx) error {
	ctx := c.Context()

	// The authenticated identity is the source of truth. A :userID in the path
	// is only ever a redundant restatement of it -- it is never trusted on its
	// own, or any caller could read any other user's cart.
	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || tokenUser == nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	userID := tokenUser.UserID

	if userIDParam := c.Params("userID"); userIDParam != "" {
		requested, err := primitive.ObjectIDFromHex(userIDParam)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"success": false,
				"message": "Invalid user ID format",
				"error":   err.Error(),
			})
		}
		// Same rule as RemoveFromCart: own cart, or admin.
		if requested != tokenUser.UserID && tokenUser.Role != "admin" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"success": false,
				"message": "Not authorized to view this cart",
			})
		}
		userID = requested
	}

	// Check if the cart is in Redis cache
	cacheKey := fmt.Sprintf("cart:%s", userID.Hex())
	var cartResponse models.CartResponse
	err := h.DB.CacheGet(ctx, cacheKey, &cartResponse)
	if err == nil {
		// Cache hit
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Cart retrieved from cache",
			"data":    cartResponse,
		})
	}

	// Find all cart items for the user
	cartCollection := h.DB.Collections().CartItems
	cursor, err := cartCollection.Find(ctx, bson.M{"user_id": userID})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to retrieve cart items",
			"error":   err.Error(),
		})
	}
	defer cursor.Close(ctx)

	// Parse the results
	var cartItems []models.CartItem
	if err := cursor.All(ctx, &cartItems); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to decode cart items",
			"error":   err.Error(),
		})
	}

	// If cart is empty
	if len(cartItems) == 0 {
		emptyCart := models.CartResponse{
			Items: []models.CartItem{},
			Total: 0,
		}

		// Cache empty cart (expire after 30 minutes)
		h.DB.CacheSet(ctx, cacheKey, emptyCart, 30*time.Minute)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"success": true,
			"message": "Cart is empty",
			"data":    emptyCart,
		})
	}

	// Fetch product details for each cart item
	productCollection := h.DB.Collections().Products
	heroCollection := h.DB.MongoDB.Collection("hero_slides")
	collectionCollection := h.DB.MongoDB.Collection("home_collection_features")
	var total float64

	for i, item := range cartItems {
		var product models.Product
		err := productCollection.FindOne(ctx, bson.M{"_id": item.ProductID}).Decode(&product)
		if err == nil {
			cartItems[i].Product = &product
			// Use discounted price if active
			total += product.GetFinalPrice() * float64(item.Quantity)
		} else if err == mongo.ErrNoDocuments {
			// Try to find in home content by productId
			var heroSlide models.HeroSlide
			err = heroCollection.FindOne(ctx, bson.M{"productId": item.ProductID}).Decode(&heroSlide)
			if err == nil {
				// Convert hero slide to product format
				priceFloat := 0.0
				fmt.Sscanf(heroSlide.Price, "₹%f", &priceFloat)
				if priceFloat == 0 {
					// Try without currency symbol
					fmt.Sscanf(heroSlide.Price, "%f", &priceFloat)
				}

				product = models.Product{
					ID:          *heroSlide.ProductID,
					Name:        heroSlide.Title,
					Description: heroSlide.Description,
					Price:       priceFloat,
					ImageURL:    heroSlide.Image,
					Images:      []string{heroSlide.Image},
					Stock:       999, // Home content products have unlimited stock
				}
				cartItems[i].Product = &product
				total += priceFloat * float64(item.Quantity)
			} else {
				// Try collection features
				var collectionFeature models.HomeCollectionFeature
				err = collectionCollection.FindOne(ctx, bson.M{"productId": item.ProductID}).Decode(&collectionFeature)
				if err == nil {
					priceFloat := 0.0
					if collectionFeature.Price != "" {
						fmt.Sscanf(collectionFeature.Price, "₹%f", &priceFloat)
						if priceFloat == 0 {
							fmt.Sscanf(collectionFeature.Price, "%f", &priceFloat)
						}
					}

					product = models.Product{
						ID:          *collectionFeature.ProductID,
						Name:        collectionFeature.Title,
						Description: collectionFeature.Description,
						Price:       priceFloat,
						ImageURL:    collectionFeature.Image,
						Images:      []string{collectionFeature.Image},
						Stock:       999,
					}
					cartItems[i].Product = &product
					total += priceFloat * float64(item.Quantity)
				}
			}
		}
	}

	// Create cart response
	cartResponse = models.CartResponse{
		Items: cartItems,
		Total: total,
	}

	// Cache the cart (expire after 30 minutes)
	h.DB.CacheSet(ctx, cacheKey, cartResponse, 30*time.Minute)

	// Return the cart
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Cart retrieved successfully",
		"data":    cartResponse,
	})
}

// RemoveFromCart removes an item from the cart
func (h *CartHandler) RemoveFromCart(c *fiber.Ctx) error {
	ctx := c.Context()

	// Get user ID and product ID from URL parameters
	userIDParam := c.Params("userID")
	productIDParam := c.Params("productID")

	if userIDParam == "" || productIDParam == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "User ID and product ID are required",
		})
	}

	// Convert IDs from string to ObjectID
	userID, err := primitive.ObjectIDFromHex(userIDParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid user ID format",
			"error":   err.Error(),
		})
	}

	productID, err := primitive.ObjectIDFromHex(productIDParam)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid product ID format",
			"error":   err.Error(),
		})
	}

	// Check if the user is authorized to remove this item
	tokenUser, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok || tokenUser == nil || (tokenUser.UserID != userID && tokenUser.Role != "admin") {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"success": false,
			"message": "Not authorized to modify this cart",
		})
	}

	// Remove the item from the cart
	cartCollection := h.DB.Collections().CartItems
	result, err := cartCollection.DeleteOne(ctx, bson.M{
		"user_id":    userID,
		"product_id": productID,
	})

	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to remove item from cart",
			"error":   err.Error(),
		})
	}

	if result.DeletedCount == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "Item not found in cart",
		})
	}

	// Invalidate cart cache
	cacheKey := fmt.Sprintf("cart:%s", userID.Hex())
	h.DB.CacheDel(ctx, cacheKey)

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Item removed from cart successfully",
	})
}

// ReplaceCart replaces the caller's whole cart with the submitted lines.
//
// PUT /cart. The storefront keeps a client-side bag so a signed-out visitor can
// shop; checkout, however, is built entirely from the server cart -- both
// /checkout and /payments/razorpay/order price the order from it and never
// trust a client total. This is the endpoint that reconciles the two, so it has
// to be idempotent: POST /cart adds to the existing quantity, which would
// double the bag if sync ran twice.
//
// A line the server cannot honour is adjusted rather than fatal. One product
// selling out while it sat in someone's bag must not reject the rest of the
// cart; the applied quantity and the reason come back so the customer can be
// told what changed before they pay.
func (h *CartHandler) ReplaceCart(c *fiber.Ctx) error {
	ctx := c.Context()

	user, ok := c.Locals("user").(*middleware.TokenMetadata)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"success": false,
			"message": "Unauthorized - User data not found",
		})
	}

	var req models.CartReplaceRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "Invalid request body",
			"error":   err.Error(),
		})
	}

	// One query for every product referenced, rather than one per line.
	products := map[primitive.ObjectID]models.Product{}
	if ids := models.CartProductIDs(req.Items); len(ids) > 0 {
		cursor, err := h.DB.Collections().Products.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to price the cart",
				"error":   err.Error(),
			})
		}
		defer cursor.Close(ctx)

		var found []models.Product
		if err := cursor.All(ctx, &found); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to decode products",
				"error":   err.Error(),
			})
		}
		for _, product := range found {
			products[product.ID] = product
		}
	}

	plan, adjustments := models.PlanCart(req.Items, products)

	now := time.Now()
	items := make([]models.CartItem, 0, len(plan))
	for _, line := range plan {
		items = append(items, models.CartItem{
			ID:        primitive.NewObjectID(),
			UserID:    user.UserID,
			ProductID: line.ProductID,
			Size:      line.Size,
			Quantity:  line.Quantity,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	cartCollection := h.DB.Collections().CartItems
	if _, err := cartCollection.DeleteMany(ctx, bson.M{"user_id": user.UserID}); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"success": false,
			"message": "Failed to clear the existing cart",
			"error":   err.Error(),
		})
	}

	if len(items) > 0 {
		docs := make([]interface{}, len(items))
		for i := range items {
			docs[i] = items[i]
		}
		if _, err := cartCollection.InsertMany(ctx, docs); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"success": false,
				"message": "Failed to store the cart",
				"error":   err.Error(),
			})
		}
	}

	h.DB.CacheDel(ctx, fmt.Sprintf("cart:%s", user.UserID.Hex()))

	// Return the stored cart priced the way checkout will price it, so the
	// client can show the real figures instead of its own arithmetic.
	response := models.CartSyncResponse{
		Items:       []models.CartItem{},
		Total:       0,
		Adjustments: adjustments,
	}
	for i := range items {
		product := products[items[i].ProductID]
		items[i].Product = &product
		response.Items = append(response.Items, items[i])
		response.Total += product.GetFinalPrice() * float64(items[i].Quantity)
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"success": true,
		"message": "Cart updated",
		"data":    response,
	})
}
