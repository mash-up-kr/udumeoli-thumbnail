package image

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG format
	"io"
	"net/http"

	"github.com/nfnt/resize"
)

// Processor handles image downloading and resizing
type Processor struct {
	MaxWidth  uint
	MaxHeight uint
}

// NewProcessor creates a new image processor
func NewProcessor(maxWidth, maxHeight uint) *Processor {
	return &Processor{
		MaxWidth:  maxWidth,
		MaxHeight: maxHeight,
	}
}

// GenerateThumbnail downloads an image from a URL and generates a thumbnail
func (p *Processor) GenerateThumbnail(imageURL string) (io.Reader, error) {
	// Download the image
	resp, err := http.Get(imageURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	// Decode the image
	img, _, err := image.Decode(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	// Resize the image to fit within MaxWidth x MaxHeight while preserving aspect ratio
	thumb := resize.Thumbnail(p.MaxWidth, p.MaxHeight, img, resize.Lanczos3)

	// Encode the thumbnail back to JPEG
	buf := new(bytes.Buffer)
	err = jpeg.Encode(buf, thumb, &jpeg.Options{Quality: 85})
	if err != nil {
		return nil, fmt.Errorf("failed to encode thumbnail: %w", err)
	}

	return buf, nil
}
