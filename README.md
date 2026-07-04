# Thumbnail Generator

This is a Go application that provides a REST API to download an image from a URL, generate a thumbnail (up to 480x480), upload it to Oracle Cloud Infrastructure (OCI) Object Storage, and save the metadata to an Oracle Autonomous Database.

## Features
- High-quality image resizing using `Lanczos3` algorithm.
- Direct upload to OCI Object Storage.
- Metadata storage in Oracle Autonomous DB (using pure Go driver `go-ora`).
- Auto-initialization of the database schema.

## Prerequisites
- Go 1.20 or higher
- OCI API Key and configured `~/.oci/config`
- Oracle Autonomous Database

## Environment Variables

The application requires the following environment variables to run:

### Required
* `DB_DSN`: Oracle Database connection string.
  * **Format**: `oracle://username:password@host:port/service`
  * **Example**: `oracle://admin:Secret123@adb.ap-seoul-1.oraclecloud.com:1522/my_atp_high`
* `OCI_BUCKET_NAME`: The name of the OCI Object Storage bucket where thumbnails will be saved.
  * **Example**: `thumbnail-bucket`

### Optional
* `PORT`: The port on which the REST API server will run. Defaults to `8080`.

*(Note: If you are not using `~/.oci/config`, you must provide the standard OCI SDK environment variables such as `OCI_TENANCY_OCID`, `OCI_USER_OCID`, `OCI_REGION`, `OCI_FINGERPRINT`, and `OCI_PRIVATE_KEY_FILE`.)*

## How to Run

1. Clone the repository.
2. Set the environment variables:
   ```bash
   export DB_DSN="oracle://..."
   export OCI_BUCKET_NAME="your-bucket-name"
   ```
3. Run the application:
   ```bash
   go run main.go
   ```

## API Usage

**POST** `/thumbnail`

**Request:**
```json
{
  "image_url": "https://example.com/sample-image.jpg"
}
```

**Response:**
```json
{
  "id": 1,
  "original_url": "https://example.com/sample-image.jpg",
  "thumbnail_url": "https://objectstorage.region.oraclecloud.com/n/namespace/b/bucket/o/thumb_123456789.jpg"
}
```
