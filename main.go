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
	ID       int64  `json:"id"`
	ImageURL string `json:"image_url"`
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

	go func() {
		log.Println("Background worker started. Waiting for jobs...")
		for req := range queue {
			log.Printf("Worker: Processing thumbnail for ID %d", req.ID)
			ctx := context.Background()

			// 1. Fetch info from DB
			originalURL, _, encryptedKeyNull, _, err := database.GetImageInfo(req.ID)
			if err != nil {
				log.Printf("Worker: Failed to get image info for ID %d: %v", req.ID, err)
				continue
			}

			// 2. Decrypt DEK if present
			var plainDEK []byte
			if encryptedKeyNull.Valid && encryptedKeyNull.String != "" {
				if len(kmsMasterKey) != 32 {
					log.Printf("Worker: KMS_MASTER_KEY is invalid, cannot decrypt image ID %d", req.ID)
					continue
				}
				plainDEK, err = crypto.DecryptDEK(encryptedKeyNull.String, kmsMasterKey)
				if err != nil {
					log.Printf("Worker: Failed to decrypt DEK for ID %d: %v", req.ID, err)
					continue
				}
			}

			// Extract original key
			originalKey := extractObjectKey(originalURL)

			// 3. Download original image using OCI SDK (with or without SSE-C)
			imgReader, err := ociStorage.DownloadImage(ctx, originalKey, plainDEK)
			if err != nil {
				log.Printf("Worker: Failed to download image for ID %d from key %s: %v", req.ID, originalKey, err)
				continue
			}

			// 4. Generate Thumbnail in memory
			thumbReader, err := processor.GenerateThumbnail(imgReader)
			imgReader.Close() // Close the original image reader after processing
			if err != nil {
				log.Printf("Worker: Failed to generate thumbnail for ID %d: %v", req.ID, err)
				continue
			}

			// 5. Upload Thumbnail to OCI using the same DEK (SSE-C)
			thumbnailKey := fmt.Sprintf("thumb_%d_%d.jpg", req.ID, time.Now().UnixNano())
			thumbnailURL, err := ociStorage.UploadThumbnail(ctx, thumbnailKey, thumbReader, plainDEK)
			if err != nil {
				log.Printf("Worker: Failed to upload thumbnail for ID %d: %v", req.ID, err)
				continue
			}

			// 6. Save to Database (UPDATE)
			if err := database.UpdateThumbnail(req.ID, thumbnailURL, thumbnailKey); err != nil {
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

		idStr := strings.TrimPrefix(r.URL.Path, "/stream/")
		if idStr == "" {
			http.Error(w, "Missing image ID", http.StatusBadRequest)
			return
		}

		expires := r.URL.Query().Get("expires")
		sig := r.URL.Query().Get("sig")
		if !isValidSignature(idStr, expires, sig, kmsMasterKey) {
			http.Error(w, "Unauthorized or expired link", http.StatusUnauthorized)
			return
		}

		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "Invalid image ID", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		originalURL, _, encryptedKeyNull, thumbnailURLNull, err := database.GetImageInfo(id)
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
