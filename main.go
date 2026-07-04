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

	"github.com/joho/godotenv"
)

type ProcessRequest struct {
	ImageURL string `json:"image_url"`
}

type ProcessResponse struct {
	ID           int64  `json:"id"`
	OriginalURL  string `json:"original_url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

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
	queue := make(chan string, 100)

	// Start a single background worker goroutine
	// This ensures requests are processed strictly one by one
	go func() {
		log.Println("Background worker started. Waiting for jobs...")
		for imageURL := range queue {
			log.Printf("Worker: Processing thumbnail for %s", imageURL)

			// Context with timeout for individual job (optional, but good practice)
			ctx := context.Background()

			// 1. Generate Thumbnail
			thumbReader, err := processor.GenerateThumbnail(imageURL)
			if err != nil {
				log.Printf("Worker: Failed to generate thumbnail for %s: %v", imageURL, err)
				continue
			}

			// 2. Upload to OCI
			objectName := fmt.Sprintf("thumb_%d.jpg", time.Now().UnixNano())
			thumbnailURL, err := ociStorage.UploadThumbnail(ctx, objectName, thumbReader)
			if err != nil {
				log.Printf("Worker: Failed to upload to OCI for %s: %v", imageURL, err)
				continue
			}

			// 3. Save to Database
			id, err := database.SaveThumbnail(imageURL, thumbnailURL)
			if err != nil {
				log.Printf("Worker: Failed to save to database for %s: %v", imageURL, err)
				continue
			}

			log.Printf("Worker: Successfully processed %s. DB ID: %d", imageURL, id)
		}
	}()

	// Set up HTTP handler
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

		if req.ImageURL == "" {
			http.Error(w, "image_url is required", http.StatusBadRequest)
			return
		}

		// Push to queue
		select {
		case queue <- req.ImageURL:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "accepted",
				"message": "Thumbnail processing queued",
			})
			log.Printf("Enqueued request for %s", req.ImageURL)
		default:
			// Queue is full
			http.Error(w, "Server is busy, try again later", http.StatusServiceUnavailable)
			log.Printf("Queue full. Rejected request for %s", req.ImageURL)
		}
	})

	// Start server
	log.Printf("Server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
