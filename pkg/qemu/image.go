package qemu

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
)

const (
	// DefaultImageCacheDir is the default directory for cached images
	DefaultImageCacheDir = "/var/lib/stargate/images"

	// UbuntuCloudImageURL is the URL for Ubuntu 22.04 cloud image
	UbuntuCloudImageURL = "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
)

// ImageManager handles downloading and caching of VM base images
type ImageManager struct {
	CacheDir string
	Logger   logr.Logger
}

// NewImageManager creates a new ImageManager
func NewImageManager(cacheDir string, logger logr.Logger) *ImageManager {
	if cacheDir == "" {
		cacheDir = DefaultImageCacheDir
	}
	return &ImageManager{
		CacheDir: cacheDir,
		Logger:   logger.WithName("image"),
	}
}

// EnsureImage downloads an image if not already cached and returns the path
func (im *ImageManager) EnsureImage(ctx context.Context, imageURL string) (string, error) {
	if imageURL == "" {
		imageURL = UbuntuCloudImageURL
	}

	// Create cache directory
	if err := os.MkdirAll(im.CacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Generate cache filename from URL hash
	hash := sha256.Sum256([]byte(imageURL))
	filename := fmt.Sprintf("%x.qcow2", hash[:8])
	imagePath := filepath.Join(im.CacheDir, filename)

	// Check if already cached
	if _, err := os.Stat(imagePath); err == nil {
		im.Logger.Info("Image already cached", "path", imagePath)
		return imagePath, nil
	}

	// Download image
	im.Logger.Info("Downloading image", "url", imageURL, "path", imagePath)

	if err := im.downloadImage(ctx, imageURL, imagePath); err != nil {
		return "", fmt.Errorf("failed to download image: %w", err)
	}

	im.Logger.Info("Image downloaded successfully", "path", imagePath)
	return imagePath, nil
}

// downloadImage downloads an image from URL to the given path
func (im *ImageManager) downloadImage(ctx context.Context, url, destPath string) error {
	// Create temp file for download
	tempPath := destPath + ".tmp"
	defer os.Remove(tempPath) // Clean up temp file on error

	// Create destination file
	out, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer out.Close()

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Execute request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Copy with progress logging
	size := resp.ContentLength
	im.Logger.Info("Starting download", "size", formatBytes(size))

	written, err := io.Copy(out, &progressReader{
		reader: resp.Body,
		total:  size,
		logger: im.Logger,
	})
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	im.Logger.Info("Download complete", "written", formatBytes(written))

	// Close before rename
	out.Close()

	// Rename temp file to final path
	if err := os.Rename(tempPath, destPath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// progressReader wraps a reader and logs progress
type progressReader struct {
	reader  io.Reader
	total   int64
	read    int64
	lastLog int64
	logger  logr.Logger
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)

	// Log every 10%
	if pr.total > 0 {
		progress := (pr.read * 100) / pr.total
		lastProgress := (pr.lastLog * 100) / pr.total
		if progress/10 > lastProgress/10 {
			pr.logger.Info("Download progress", "percent", progress, "downloaded", formatBytes(pr.read))
			pr.lastLog = pr.read
		}
	}

	return n, err
}

func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

// GetCachedImagePath returns the path for a cached image without downloading
func (im *ImageManager) GetCachedImagePath(imageURL string) string {
	if imageURL == "" {
		imageURL = UbuntuCloudImageURL
	}
	hash := sha256.Sum256([]byte(imageURL))
	filename := fmt.Sprintf("%x.qcow2", hash[:8])
	return filepath.Join(im.CacheDir, filename)
}

// IsCached checks if an image is already cached
func (im *ImageManager) IsCached(imageURL string) bool {
	path := im.GetCachedImagePath(imageURL)
	_, err := os.Stat(path)
	return err == nil
}
