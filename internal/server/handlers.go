package server

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/dustin/go-humanize"
	"github.com/gofiber/fiber/v2"
	"github.com/watzon/paste69/internal/models"
	"go.uber.org/zap"
)

func (s *Server) handleIndex(c *fiber.Ctx) error {
	// Get current stats
	var totalPastes, totalUrls int64
	s.db.Model(&models.Paste{}).Count(&totalPastes)
	s.db.Model(&models.Shortlink{}).Count(&totalUrls)

	// Get historical data
	history, err := s.getStatsHistory(7) // Last 7 days
	if err != nil {
		s.logger.Error("failed to get stats history", zap.Error(err))
		// Continue without history
	}

	// Convert history to JSON strings
	pastesHistory, _ := json.Marshal(history.Pastes)
	urlsHistory, _ := json.Marshal(history.URLs)
	storageHistory, _ := json.Marshal(history.Storage)

	return c.Render("index", fiber.Map{
		"stats": fiber.Map{
			"pastes":         totalPastes,
			"urls":           totalUrls,
			"storage":        humanize.IBytes(s.getStorageSize()),
			"pastesHistory":  string(pastesHistory),
			"urlsHistory":    string(urlsHistory),
			"storageHistory": string(storageHistory),
		},
		"retention": fiber.Map{
			"noKey":   s.config.Server.Cleanup.MaxAge,
			"withKey": "unlimited",
			"maxSize": humanize.IBytes(uint64(s.config.Server.MaxUploadSize)),
		},
		"baseUrl": s.config.Server.BaseURL,
	}, "layouts/main")
}

func (s *Server) handleDocs(c *fiber.Ctx) error {
	return c.Render("docs", fiber.Map{
		"baseUrl": s.config.Server.BaseURL,
		"maxSize": humanize.IBytes(uint64(s.config.Server.MaxUploadSize)),
		"retention": fiber.Map{
			"noKey":   s.config.Server.Cleanup.MaxAge,
			"withKey": "unlimited",
		},
	}, "layouts/main")
}

// Upload Handlers

// handleMultipartUpload handles file uploads via multipart/form-data
// Accepts: multipart/form-data with 'file' field
// Optional query params: ext, expires, private, filename
func (s *Server) handleMultipartUpload(c *fiber.Ctx) error {
	file, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "No file provided")
	}

	// Get optional parameters
	extension := c.Query("ext")
	expiresIn := c.Query("expires")
	private := c.QueryBool("private", false)
	filename := c.Query("filename", file.Filename)

	// Create paste
	paste, err := s.createPasteFromMultipart(c, file, &PasteOptions{
		Extension: extension,
		ExpiresIn: expiresIn,
		Private:   private,
		Filename:  filename,
	})
	if err != nil {
		return err
	}

	response := paste.ToResponse()
	s.addBaseURLToPasteResponse(response)

	return c.JSON(fiber.Map{
		"success": true,
		"data":    response,
	})
}

// handleRawUpload handles raw body uploads (direct file content)
// Accepts: any content type
// Optional query params: ext, expires, private, filename
// Content-Type header is used for mime-type detection
func (s *Server) handleRawUpload(c *fiber.Ctx) error {
	content := c.Body()
	if len(content) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "Empty content")
	}

	// Get optional parameters
	extension := c.Query("ext")
	expiresIn := c.Query("expires")
	private := c.QueryBool("private", false)
	filename := c.Query("filename", "paste")

	// Create paste from raw content
	paste, err := s.createPasteFromRaw(c, content, &PasteOptions{
		Extension: extension,
		ExpiresIn: expiresIn,
		Private:   private,
		Filename:  filename,
		APIKey:    c.Locals("apiKey").(*models.APIKey), // Will be nil if no API key
	})
	if err != nil {
		return err
	}

	response := paste.ToResponse()
	s.addBaseURLToPasteResponse(response)

	return c.JSON(fiber.Map{
		"success": true,
		"data":    response,
	})
}

// handleJSONUpload handles JSON payload uploads
// Accepts: application/json with structure:
//
//	{
//	  "content": "string",    // Required if url not provided
//	  "url": "string",        // Required if content not provided
//	  "filename": "string",   // Optional
//	  "extension": "string",  // Optional
//	  "expires_in": "string", // Optional (e.g., "24h")
//	  "private": boolean      // Optional
//	}
func (s *Server) handleJSONUpload(c *fiber.Ctx) error {
	var req struct {
		Content   string `json:"content"`
		URL       string `json:"url"`
		Filename  string `json:"filename"`
		Extension string `json:"extension"`
		ExpiresIn string `json:"expires_in"`
		Private   bool   `json:"private"`
	}

	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "Invalid JSON")
	}

	// Get API key if present
	apiKey, _ := c.Locals("apiKey").(*models.APIKey)

	opts := &PasteOptions{
		Extension: req.Extension,
		ExpiresIn: req.ExpiresIn,
		Private:   req.Private,
		Filename:  req.Filename,
		APIKey:    apiKey,
	}

	var paste *models.Paste
	var err error

	if req.URL != "" {
		paste, err = s.createPasteFromURL(c, req.URL, opts)
	} else if req.Content != "" {
		paste, err = s.createPasteFromRaw(c, []byte(req.Content), opts)
	} else {
		return fiber.NewError(fiber.StatusBadRequest, "Either content or URL must be provided")
	}

	if err != nil {
		return err
	}

	response := paste.ToResponse()
	s.addBaseURLToPasteResponse(response)

	return c.JSON(fiber.Map{
		"success": true,
		"data":    response,
	})
}

// URL Shortener Handlers

// handleURLShorten creates a new shortened URL (requires API key)
// Accepts: application/json with structure:
//
//	{
//	  "url": "string",       // Required
//	  "title": "string",     // Optional
//	  "expires_in": "string" // Optional
//	}
func (s *Server) handleURLShorten(c *fiber.Ctx) error {
	apiKey := c.Locals("apiKey").(*models.APIKey)
	if !apiKey.AllowShortlinks {
		return fiber.NewError(fiber.StatusForbidden, "API key does not allow URL shortening")
	}

	var req struct {
		URL       string `json:"url"`
		Title     string `json:"title"`
		ExpiresIn string `json:"expires_in"`
	}

	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "Invalid JSON")
	}

	// Add in handleURLShorten before creating shortlink
	if err := s.rateLimiter.Allow(apiKey.Key); err != nil {
		return fiber.NewError(fiber.StatusTooManyRequests, "Rate limit exceeded")
	}

	shortlink, err := s.createShortlink(req.URL, &ShortlinkOptions{
		Title:     req.Title,
		ExpiresIn: req.ExpiresIn,
		APIKey:    apiKey,
	})
	if err != nil {
		return err
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data":    shortlink.ToResponse(),
	})
}

// handleURLStats returns statistics for a shortened URL (requires API key)
// Returns: view count, last viewed, etc.
func (s *Server) handleURLStats(c *fiber.Ctx) error {
	apiKey := c.Locals("apiKey").(*models.APIKey)
	id := c.Params("id")

	shortlink, err := s.findShortlink(id)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "Shortlink not found")
	}

	// Check if API key owns this shortlink
	if shortlink.APIKey != apiKey.Key {
		return fiber.NewError(fiber.StatusForbidden, "Not authorized to view these stats")
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data": fiber.Map{
			"id":         shortlink.ID,
			"url":        shortlink.TargetURL,
			"title":      shortlink.Title,
			"clicks":     shortlink.Clicks,
			"created_at": shortlink.CreatedAt,
			"last_click": shortlink.LastClick,
			"expires_at": shortlink.ExpiresAt,
		},
	})
}

// Management Handlers

// handleListPastes returns a list of pastes for the API key
// Optional query params: page, limit, sort
func (s *Server) handleListPastes(c *fiber.Ctx) error {
	apiKey := c.Locals("apiKey").(*models.APIKey)

	// Get pagination params
	page := c.QueryInt("page", 1)
	limit := c.QueryInt("limit", 20)
	sort := c.Query("sort", "created_at desc")

	var pastes []models.Paste
	var total int64

	// Build query
	query := s.db.Model(&models.Paste{}).Where("api_key = ?", apiKey.Key)

	// Get total count
	query.Count(&total)

	// Get paginated results
	err := query.Order(sort).
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&pastes).Error

	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to fetch pastes")
	}

	// Convert to response format
	var items []fiber.Map
	for _, paste := range pastes {
		response := paste.ToResponse()
		s.addBaseURLToPasteResponse(response)
		items = append(items, response)
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data": fiber.Map{
			"items": items,
			"total": total,
			"page":  page,
			"limit": limit,
		},
	})
}

// handleDeletePaste deletes a paste (requires API key ownership)
func (s *Server) handleDeletePaste(c *fiber.Ctx) error {
	id := c.Params("id")
	apiKey := c.Locals("apiKey").(*models.APIKey)

	// Find paste
	paste, err := s.findPaste(id)
	if err != nil {
		return err
	}

	// Check ownership
	if paste.APIKey != apiKey.Key {
		return fiber.NewError(fiber.StatusForbidden, "Not authorized to delete this paste")
	}

	// Delete from storage first
	if err := s.store.Delete(paste.StoragePath); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to delete paste content")
	}

	// Delete from database
	if err := s.db.Delete(paste).Error; err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to delete paste record")
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Paste deleted successfully",
	})
}

// handleUpdateExpiration updates a paste's expiration time
// Accepts: application/json with structure:
//
//	{
//	  "expires_in": "string" // Required (e.g., "24h", or "never")
//	}
func (s *Server) handleUpdateExpiration(c *fiber.Ctx) error {
	id := c.Params("id")
	apiKey := c.Locals("apiKey").(*models.APIKey)

	var req struct {
		ExpiresIn string `json:"expires_in"`
	}

	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "Invalid JSON")
	}

	// Find paste
	paste, err := s.findPaste(id)
	if err != nil {
		return err
	}

	// Check ownership
	if paste.APIKey != apiKey.Key {
		return fiber.NewError(fiber.StatusForbidden, "Not authorized to modify this paste")
	}

	// Update expiration
	if req.ExpiresIn == "never" {
		paste.ExpiresAt = nil
	} else {
		expiry, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Invalid expiration format")
		}
		expiryTime := time.Now().Add(expiry)
		paste.ExpiresAt = &expiryTime
	}

	// Save changes
	if err := s.db.Save(paste).Error; err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to update paste")
	}

	response := paste.ToResponse()
	s.addBaseURLToPasteResponse(response)

	return c.JSON(fiber.Map{
		"success": true,
		"data":    response,
	})
}

// Public Access Handlers

// handleView serves the content with syntax highlighting if applicable
// For URLs, redirects to target URL and increments view counter
func (s *Server) handleView(c *fiber.Ctx) error {
	id := c.Params("id")

	// Try shortlink first
	if shortlink, err := s.findShortlink(id); err == nil {
		// Update click stats asynchronously
		go s.updateShortlinkStats(shortlink, c)
		return c.Redirect(shortlink.TargetURL, fiber.StatusTemporaryRedirect)
	}

	// Try paste
	paste, err := s.findPaste(id)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	c.Set("Cache-Control", "public, max-age=300") // Cache for 5 minutes

	// Handle view based on content type
	if isTextContent(paste.MimeType) {
		return s.renderPasteView(c, paste)
	}

	return c.Redirect("/download/"+id, fiber.StatusTemporaryRedirect)
}

// handleRawView serves the raw content of a paste
func (s *Server) handleRawView(c *fiber.Ctx) error {
	id := c.Params("id")

	paste, err := s.findPaste(id)
	if err != nil {
		return err
	}

	// Get content from storage
	content, err := s.store.Get(paste.StoragePath)
	if err != nil {
		s.logger.Error("failed to read content from storage",
			zap.String("id", id),
			zap.Error(err),
		)
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to read content")
	}
	defer content.Close()

	// For text content, use text/plain to display in browser
	contentType := paste.MimeType
	if isTextContent(paste.MimeType) {
		contentType = "text/plain; charset=utf-8"
	}

	// Set content type and cache headers
	c.Set("Content-Type", contentType)
	c.Set("Content-Length", fmt.Sprintf("%d", paste.Size))
	c.Set("Cache-Control", "public, max-age=300") // Cache for 5 minutes

	// Read all content first
	data, err := io.ReadAll(content)
	if err != nil {
		s.logger.Error("failed to read content",
			zap.String("id", id),
			zap.Error(err),
		)
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to read content")
	}

	return c.Send(data)
}

// handleDownload serves the content as a downloadable file
func (s *Server) handleDownload(c *fiber.Ctx) error {
	id := c.Params("id")

	paste, err := s.findPaste(id)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "Not found")
	}

	// Get content from storage
	content, err := s.store.Get(paste.StoragePath)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to read content")
	}
	defer content.Close()

	// Read all content first
	data, err := io.ReadAll(content)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to read content")
	}

	// Set download headers
	c.Set("Content-Type", "application/octet-stream")
	c.Set("Content-Length", fmt.Sprintf("%d", paste.Size))
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, paste.Filename))
	c.Set("Cache-Control", "public, max-age=300") // Cache for 5 minutes

	return c.Send(data)
}

// handleDeleteWithKey deletes a paste using its deletion key
// No authentication required, but deletion key must match
func (s *Server) handleDeleteWithKey(c *fiber.Ctx) error {
	id := c.Params("id")
	key := c.Params("key")

	paste, err := s.findPaste(id)
	if err != nil {
		return err
	}

	if paste.DeleteKey != key {
		return fiber.NewError(fiber.StatusForbidden, "Invalid delete key")
	}

	// Delete from storage first
	if err := s.store.Delete(paste.StoragePath); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to delete paste content")
	}

	// Delete from database
	if err := s.db.Delete(paste).Error; err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to delete paste record")
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Paste deleted successfully",
	})
}

// renderPasteView renders the paste view for text content
func (s *Server) renderPasteView(c *fiber.Ctx, paste *models.Paste) error {
	// Get content from storage
	content, err := s.store.Get(paste.StoragePath)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to read content")
	}
	defer content.Close()

	// Read all content
	data, err := io.ReadAll(content)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to read content")
	}

	// Get lexer based on extension or mime type
	lexer := lexers.Get(paste.Extension)
	if lexer == nil {
		// Try to match by filename
		lexer = lexers.Match(paste.Filename)
		if lexer == nil {
			// Try to analyze content
			lexer = lexers.Analyse(string(data))
			if lexer == nil {
				lexer = lexers.Fallback
			}
		}
	}
	lexer = chroma.Coalesce(lexer)

	// Create formatter without classes (will use inline styles)
	formatter := html.New(
		html.WithLineNumbers(true),
		html.WithLinkableLineNumbers(true, ""),
		html.TabWidth(4),
	)

	// Use gruvbox style (dark theme that matches our UI)
	style := styles.Get("gruvbox")
	if style == nil {
		style = styles.Fallback
	}

	// Generate highlighted HTML
	var highlightedContent strings.Builder
	iterator, err := lexer.Tokenise(nil, string(data))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to tokenize content")
	}

	err = formatter.Format(&highlightedContent, style, iterator)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to format content")
	}

	return c.Render("paste", fiber.Map{
		"id":       paste.ID,
		"filename": paste.Filename,
		"content":  highlightedContent.String(),
		"language": lexer.Config().Name,
		"created":  paste.CreatedAt.Format(time.RFC3339),
		"expires":  paste.ExpiresAt,
	}, "layouts/main")
}

// getStorageSize returns total size of stored files
func (s *Server) getStorageSize() uint64 {
	var total uint64
	s.db.Model(&models.Paste{}).
		Select("COALESCE(SUM(size), 0)").
		Row().
		Scan(&total)
	return total
}

// Add this helper method to the Server struct
func (s *Server) addBaseURLToPasteResponse(response fiber.Map) {
	baseURL := strings.TrimSuffix(s.config.Server.BaseURL, "/")
	for key, value := range response {
		if strValue, ok := value.(string); ok {
			if strings.HasSuffix(key, "_url") {
				response[key] = baseURL + strValue
			}
		}
	}
}