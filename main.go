package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"strconv"
	"strings"
	"time"

	"thumbnailer/internal/crypto"
	"thumbnailer/internal/db"
	"thumbnailer/internal/image"
	"thumbnailer/internal/storage"

	_ "thumbnailer/docs"
	"github.com/swaggo/http-swagger"
	"github.com/joho/godotenv"
)

type ProcessRequest struct {
	ID         int64  `json:"id"`
	ImageURL   string `json:"image_url"`
	RetryCount int    `json:"-"`
}

type ProcessResponse struct {
	ID           int64  `json:"id"`
	OriginalURL  string `json:"original_url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

func extractObjectKey(fullURL string) string {
	parts := strings.SplitN(fullURL, "/o/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	parts = strings.Split(fullURL, "/")
	return parts[len(parts)-1]
}

func isValidSignature(imageId string, expiresStr string, receivedSig string, masterKey []byte) bool {
	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return false
	}

	payload := fmt.Sprintf("%s:%s", imageId, expiresStr)
	h := hmac.New(sha256.New, masterKey)
	h.Write([]byte(payload))
	expectedSigBytes := h.Sum(nil)

	expectedSig := base64.RawURLEncoding.EncodeToString(expectedSigBytes)
	return expectedSig == receivedSig
}

// @title Nifty Galileo Thumbnail API
// @version 1.0
// @description This is an asynchronous thumbnail generation API.
// @host localhost:8080
// @BasePath /
func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found or error reading it, relying on system environment variables")
	}

	dbDSN := os.Getenv("DB_DSN")
	if dbDSN == "" {
		log.Fatal("DB_DSN environment variable is required. Format: oracle://user:password@host:port/service")
	}

	bucketName := os.Getenv("OCI_BUCKET_NAME")
	if bucketName == "" {
		log.Fatal("OCI_BUCKET_NAME environment variable is required")
	}

	kmsMasterKeyStr := os.Getenv("KMS_MASTER_KEY")
	if kmsMasterKeyStr == "" || len(kmsMasterKeyStr) != 32 {
		log.Println("WARNING: KMS_MASTER_KEY is not set or not 32 bytes long. SSE-C encryption/decryption will fail if required.")
	}
	kmsMasterKey := []byte(kmsMasterKeyStr)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

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

	log.Println("Initializing OCI Object Storage client...")
	ociStorage, err := storage.NewOCIStorage(bucketName)
	if err != nil {
		log.Fatalf("Failed to initialize OCI storage: %v", err)
	}

	processor := image.NewProcessor(480, 480)
	queue := make(chan ProcessRequest, 100)

	// Scheduler: Periodically fetch missed thumbnails from DB
	// We manage the retry count for these purely in-memory using a map
	var schedulerFailTracker sync.Map
	go func() {
		log.Println("Scheduler started. Polling for missed thumbnails every 5 minutes...")
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			log.Println("Scheduler: Checking for missing thumbnails...")
			ids, err := database.GetPendingThumbnails(50) // Fetch up to 50 at a time
			if err != nil {
				log.Printf("Scheduler: Failed to get pending thumbnails: %v", err)
				continue
			}

			if len(ids) == 0 {
				log.Println("Scheduler: No pending thumbnails found.")
				continue
			}
			log.Printf("Scheduler: Found %d missing thumbnails", len(ids))

			for _, id := range ids {
				// Check global tracker
				val, _ := schedulerFailTracker.LoadOrStore(id, 0)
				attempts := val.(int)
				if attempts >= 3 {
					log.Printf("Scheduler: ID %d has already failed 3 times globally in memory, skipping", id)
					continue
				}
				schedulerFailTracker.Store(id, attempts+1)

				// Don't block the scheduler if queue is full
				select {
				case queue <- ProcessRequest{ID: id, RetryCount: 0}:
					log.Printf("Scheduler: Queued missing thumbnail for ID %d", id)
				default:
					log.Printf("Scheduler: Queue full, skipped queuing ID %d for now", id)
					schedulerFailTracker.Store(id, attempts) // Revert attempt count since we didn't queue
				}
			}
		}
	}()

	go func() {
		log.Println("Background worker started. Waiting for jobs...")
		for req := range queue {
			log.Printf("Worker: Processing thumbnail for ID %d (Attempt %d)", req.ID, req.RetryCount+1)
			ctx := context.Background()

			handleFailure := func(reason string, err error, retriable bool) {
				log.Printf("Worker: %s for ID %d: %v", reason, req.ID, err)
				if !retriable {
					log.Printf("Worker: Fatal (non-retriable) error for ID %d. Giving up immediately.", req.ID)
					return
				}
				
				if req.RetryCount < 3 {
					req.RetryCount++
					go func(r ProcessRequest) {
						delay := time.Duration(r.RetryCount*5) * time.Second
						log.Printf("Worker: Retrying ID %d in %v...", r.ID, delay)
						time.Sleep(delay)
						queue <- r
					}(req)
				} else {
					log.Printf("Worker: Giving up on ID %d after %d retries", req.ID, req.RetryCount)
				}
			}

			// 1. Fetch info from DB
			originalURL, _, encryptedKeyNull, thumbnailURLNull, err := database.GetImageInfo(req.ID)
			if err != nil {
				// DB connection errors are retriable, but "no record found" is not
				isRetriable := !strings.Contains(err.Error(), "no record found")
				handleFailure("Failed to get image info", err, isRetriable)
				continue
			}

			if thumbnailURLNull.Valid && thumbnailURLNull.String != "" {
				log.Printf("Worker: Thumbnail already exists for ID %d, skipping", req.ID)
				continue
			}

			// 2. Decrypt DEK if present
			var plainDEK []byte
			if encryptedKeyNull.Valid && encryptedKeyNull.String != "" {
				if len(kmsMasterKey) != 32 {
					handleFailure("KMS_MASTER_KEY is invalid", fmt.Errorf("cannot decrypt image"), false)
					continue
				}
				plainDEK, err = crypto.DecryptDEK(encryptedKeyNull.String, kmsMasterKey)
				if err != nil {
					handleFailure("Failed to decrypt DEK", err, false)
					continue
				}
			}

			// Extract original key
			originalKey := extractObjectKey(originalURL)

			// 3. Download original image using OCI SDK (with or without SSE-C)
			imgReader, err := ociStorage.DownloadImage(ctx, originalKey, plainDEK)
			if err != nil {
				handleFailure(fmt.Sprintf("Failed to download image from key %s", originalKey), err, true)
				continue
			}

			// 4. Generate Thumbnail in memory
			thumbReader, err := processor.GenerateThumbnail(imgReader)
			imgReader.Close()
			if err != nil {
				handleFailure("Failed to generate thumbnail", err, false)
				continue
			}

			// 5. Upload Thumbnail to OCI using the same DEK (SSE-C)
			thumbnailKey := fmt.Sprintf("thumb_%d_%d.jpg", req.ID, time.Now().UnixNano())
			thumbnailURL, err := ociStorage.UploadThumbnail(ctx, thumbnailKey, thumbReader, plainDEK)
			if err != nil {
				handleFailure("Failed to upload thumbnail", err, true)
				continue
			}

			// 6. Save to Database (UPDATE)
			if err := database.UpdateThumbnail(req.ID, thumbnailURL, thumbnailKey); err != nil {
				handleFailure("Failed to update database", err, true)
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
			http.Error(w, "Server is busy, try again later", http.StatusServiceUnavailable)
			log.Printf("Queue full. Rejected request for ID %d", req.ID)
		}
	})

	http.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		objectKey := strings.TrimPrefix(r.URL.Path, "/stream/")
		if objectKey == "" {
			http.Error(w, "Missing image ID", http.StatusBadRequest)
			return
		}

		expires := r.URL.Query().Get("expires")
		sig := r.URL.Query().Get("sig")
		if !isValidSignature(objectKey, expires, sig, kmsMasterKey) {
			http.Error(w, "Unauthorized or expired link", http.StatusUnauthorized)
			return
		}


		ctx := r.Context()
		originalURL, _, encryptedKeyNull, thumbnailURLNull, err := database.GetImageInfoByObjectKey(objectKey)
		if err != nil {
			http.Error(w, "Image not found", http.StatusNotFound)
			return
		}

		var plainDEK []byte
		if encryptedKeyNull.Valid && encryptedKeyNull.String != "" {
			if len(kmsMasterKey) != 32 {
				http.Error(w, "Server configuration error", http.StatusInternalServerError)
				return
			}
			plainDEK, err = crypto.DecryptDEK(encryptedKeyNull.String, kmsMasterKey)
			if err != nil {
				http.Error(w, "Decryption error", http.StatusInternalServerError)
				return
			}
		}

		targetURL := originalURL
		if r.URL.Query().Get("type") == "thumb" && thumbnailURLNull.Valid && thumbnailURLNull.String != "" {
			targetURL = thumbnailURLNull.String
		}

		targetKey := extractObjectKey(targetURL)
		imgReader, err := ociStorage.DownloadImage(ctx, targetKey, plainDEK)
		if err != nil {
			http.Error(w, "Failed to fetch image from storage", http.StatusBadGateway)
			return
		}
		defer imgReader.Close()

		w.Header().Set("Cache-Control", "public, max-age=31536000")
		if strings.HasSuffix(strings.ToLower(targetKey), ".png") {
			w.Header().Set("Content-Type", "image/png")
		} else {
			w.Header().Set("Content-Type", "image/jpeg")
		}

		io.Copy(w, imgReader)
	})

	http.HandleFunc("/swagger/", httpSwagger.WrapHandler)

	log.Printf("Server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
