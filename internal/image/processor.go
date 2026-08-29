package image

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG format
	_ "golang.org/x/image/webp" // register WebP format
	"io"

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

// GenerateThumbnail generates a thumbnail from an image stream
func (p *Processor) GenerateThumbnail(r io.Reader) (io.Reader, error) {
	// Decode the image
	img, _, err := image.Decode(r)
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
