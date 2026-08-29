package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

// OCIStorage handles uploading files to OCI Object Storage
type OCIStorage struct {
	client    objectstorage.ObjectStorageClient
	namespace string
	bucket    string
}

// NewOCIStorage creates a new OCI storage client
func NewOCIStorage(bucketName string) (*OCIStorage, error) {
	// Check if local .oci/config exists
	var provider common.ConfigurationProvider
	if _, err := os.Stat(".oci/config"); err == nil {
		provider, err = common.ConfigurationProviderFromFile(".oci/config", "")
		if err != nil {
			return nil, fmt.Errorf("failed to read local .oci/config: %w", err)
		}
	} else {
		// Using default configuration provider (looks in ~/.oci/config or environment variables)
		provider = common.DefaultConfigProvider()
	}

	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("failed to create object storage client: %w", err)
	}

	ctx := context.Background()

	// Get namespace
	req := objectstorage.GetNamespaceRequest{}
	resp, err := client.GetNamespace(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get namespace: %w", err)
	}

	return &OCIStorage{
		client:    client,
		namespace: *resp.Value,
		bucket:    bucketName,
	}, nil
}

// DownloadImage downloads an image from OCI, optionally using SSE-C
func (s *OCIStorage) DownloadImage(ctx context.Context, objectName string, plainDEK []byte) (io.ReadCloser, error) {
	req := objectstorage.GetObjectRequest{
		NamespaceName: common.String(s.namespace),
		BucketName:    common.String(s.bucket),
		ObjectName:    common.String(objectName),
	}

	if plainDEK != nil {
		req.OpcSseCustomerAlgorithm = common.String("AES256")
		req.OpcSseCustomerKey = common.String(base64.StdEncoding.EncodeToString(plainDEK))
		
		hasher := sha256.New()
		hasher.Write(plainDEK)
		req.OpcSseCustomerKeySha256 = common.String(base64.StdEncoding.EncodeToString(hasher.Sum(nil)))
	}

	resp, err := s.client.GetObject(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get object: %w", err)
	}

	return resp.Content, nil
}

// UploadThumbnail uploads the thumbnail and returns its public URL, optionally using SSE-C
func (s *OCIStorage) UploadThumbnail(ctx context.Context, objectName string, data io.Reader, plainDEK []byte) (string, error) {
	ext := strings.ToLower(filepath.Ext(objectName))
	contentType := "image/jpeg"
	if ext == ".png" {
		contentType = "image/png"
	}

	req := objectstorage.PutObjectRequest{
		NamespaceName: common.String(s.namespace),
		BucketName:    common.String(s.bucket),
		ObjectName:    common.String(objectName),
		PutObjectBody: io.NopCloser(data),
		ContentType:   common.String(contentType),
	}

	if plainDEK != nil {
		req.OpcSseCustomerAlgorithm = common.String("AES256")
		req.OpcSseCustomerKey = common.String(base64.StdEncoding.EncodeToString(plainDEK))
		
		hasher := sha256.New()
		hasher.Write(plainDEK)
		req.OpcSseCustomerKeySha256 = common.String(base64.StdEncoding.EncodeToString(hasher.Sum(nil)))
	}

	_, err := s.client.PutObject(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to put object: %w", err)
	}

	// Assuming the bucket is public, construct the URL
	region := s.client.Host
	if !strings.HasPrefix(region, "http") {
		region = "https://" + region
	}

	url := fmt.Sprintf("%s/n/%s/b/%s/o/%s", region, s.namespace, s.bucket, objectName)
	return url, nil
}
