package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/sijms/go-ora/v2"
)

// Database handles connection and queries to Oracle DB
type Database struct {
	db *sql.DB
}

// ThumbnailRecord represents a record in the database
type ThumbnailRecord struct {
	ID           int64
	OriginalURL  string
	ThumbnailURL string
	CreatedAt    time.Time
}

// NewDatabase creates a new Oracle database connection
// dsn should be in the format: oracle://user:password@host:port/service
func NewDatabase(dsn string) (*Database, error) {
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &Database{db: db}, nil
}

// InitSchema creates the thumbnails table if it doesn't exist
func (d *Database) InitSchema() error {
	query := `
	DECLARE
		v_count NUMBER;
	BEGIN
		SELECT COUNT(*) INTO v_count FROM user_tables WHERE table_name = 'IMAGE';
		IF v_count = 0 THEN
			EXECUTE IMMEDIATE '
				CREATE TABLE IMAGE (
					ID NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
					ORIGINAL_URL VARCHAR2(4000) NOT NULL,
					THUMBNAIL_URL VARCHAR2(4000) NOT NULL,
					CREATED_AT TIMESTAMP DEFAULT CURRENT_TIMESTAMP
				)
			';
		END IF;
	END;`

	_, err := d.db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to init schema: %w", err)
	}
	return nil
}

// SaveThumbnail saves the thumbnail information to the database
func (d *Database) SaveThumbnail(originalURL, thumbnailURL string) (int64, error) {
	query := `INSERT INTO IMAGE (ORIGINAL_URL, THUMBNAIL_URL) VALUES (:1, :2) RETURNING ID INTO :3`

	var id int64
	_, err := d.db.Exec(query, originalURL, thumbnailURL, sql.Out{Dest: &id})
	if err != nil {
		return 0, fmt.Errorf("failed to insert thumbnail record: %w", err)
	}

	return id, nil
}

// Close closes the database connection
func (d *Database) Close() error {
	return d.db.Close()
}
