package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"thumbnailer/internal/db"
	"thumbnailer/internal/image"
	"thumbnailer/internal/storage"

	_ "thumbnailer/docs"
	"github.com/swaggo/http-swagger"
	"github.com/joho/godotenv"
)

type ProcessRequest struct {
	ID       int64  `json:"id"`
	ImageURL string `json:"image_url"`
}

type ProcessResponse struct {
	ID           int64  `json:"id"`
	OriginalURL  string `json:"original_url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

// @title Nifty Galileo Thumbnail API
// @version 1.0
// @description This is an asynchronous thumbnail generation API.
// @host localhost:8080
// @BasePath /
func main() {
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found or error reading it, relying on system environment variables")
	}

	// Initialize configurations from environment variables
	dbDSN := os.Getenv("DB_DSN")
	if dbDSN == "" {
		log.Fatal("DB_DSN environment variable is required. Format: oracle://user:password@host:port/service")
	}

	bucketName := os.Getenv("OCI_BUCKET_NAME")
	if bucketName == "" {
		log.Fatal("OCI_BUCKET_NAME environment variable is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Initialize Database
	log.Println("Connecting to Oracle Database...")
	database, err := db.NewDatabase(dbDSN)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer database.Close()

	log.Println("Initializing database schema...")
	if err := database.InitSchema(); err != nil {
		log.Fatalf("Failed to initialize database schema: %v", err)
	}

	// Initialize OCI Storage
	log.Println("Initializing OCI Object Storage client...")
	ociStorage, err := storage.NewOCIStorage(bucketName)
	if err != nil {
		log.Fatalf("Failed to initialize OCI storage: %v", err)
	}

	// Initialize Image Processor (max width 480, max height 480)
	processor := image.NewProcessor(480, 480)

	// Create a channel for queuing requests
	// Buffer size of 100 ensures we can accept up to 100 requests quickly
	queue := make(chan ProcessRequest, 100)

	// Start a single background worker goroutine
	// This ensures requests are processed strictly one by one
	go func() {
		log.Println("Background worker started. Waiting for jobs...")
		for req := range queue {
			log.Printf("Worker: Processing thumbnail for ID %d (URL: %s)", req.ID, req.ImageURL)

			// Context with timeout for individual job (optional, but good practice)
			ctx := context.Background()

			// 1. Generate Thumbnail
			thumbReader, err := processor.GenerateThumbnail(req.ImageURL)
			if err != nil {
				log.Printf("Worker: Failed to generate thumbnail for ID %d: %v", req.ID, err)
				continue
			}

			// 2. Upload to OCI
			objectName := fmt.Sprintf("thumb_%d_%d.jpg", req.ID, time.Now().UnixNano())
			thumbnailURL, err := ociStorage.UploadThumbnail(ctx, objectName, thumbReader)
			if err != nil {
				log.Printf("Worker: Failed to upload to OCI for ID %d: %v", req.ID, err)
				continue
			}

			// 3. Save to Database (UPDATE)
			if err := database.UpdateThumbnail(req.ID, thumbnailURL); err != nil {
				log.Printf("Worker: Failed to update database for ID %d: %v", req.ID, err)
				continue
			}

			log.Printf("Worker: Successfully processed ID %d", req.ID)
		}
	}()

	// @Summary Generate a thumbnail
	// @Description Queues an image for thumbnail generation and database update
	// @Accept json
	// @Produce json
	// @Param request body ProcessRequest true "Image details (id and image_url)"
	// @Success 202 {object} map[string]string "Thumbnail processing queued"
	// @Failure 400 {string} string "Invalid request body or missing fields"
	// @Failure 405 {string} string "Method not allowed"
	// @Failure 503 {string} string "Server is busy, try again later"
	// @Router /thumbnail [post]
	http.HandleFunc("/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ProcessRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.ID == 0 {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}

		if req.ImageURL == "" {
			http.Error(w, "image_url is required", http.StatusBadRequest)
			return
		}

		// Push to queue
		select {
		case queue <- req:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "accepted",
				"message": "Thumbnail processing queued",
			})
			log.Printf("Enqueued request for ID %d", req.ID)
		default:
			// Queue is full
			http.Error(w, "Server is busy, try again later", http.StatusServiceUnavailable)
			log.Printf("Queue full. Rejected request for ID %d", req.ID)
		}
	})

	// Swagger UI handler
	http.HandleFunc("/swagger/", httpSwagger.WrapHandler)

	// Start server
	log.Printf("Server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
