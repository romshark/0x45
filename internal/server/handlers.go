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
	"github.com/watzon/paste69/internal/utils"
	"go.uber.org/zap"
)

// Web Interface Handlers

// handleIndex serves the main web interface page
// Displays current statistics and historical data for pastes and URLs
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

	// Generate retention data
	retentionStats, err := utils.GenerateRetentionData(int64(s.config.Server.MaxUploadSize))
	if err != nil {
		s.logger.Error("failed to generate retention data", zap.Error(err))
		// Continue with empty retention data
	}

	// Convert data to JSON strings
	pastesHistory, _ := json.Marshal(history.Pastes)
	urlsHistory, _ := json.Marshal(history.URLs)
	storageHistory, _ := json.Marshal(history.Storage)
	noKeyHistory, _ := json.Marshal(retentionStats.Data["noKey"])
	withKeyHistory, _ := json.Marshal(retentionStats.Data["withKey"])

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
			"noKey":          retentionStats.NoKeyRange,
			"withKey":        retentionStats.WithKeyRange,
			"maxSize":        humanize.IBytes(uint64(s.config.Server.MaxUploadSize)),
			"noKeyHistory":   string(noKeyHistory),
			"withKeyHistory": string(withKeyHistory),
		},
		"baseUrl": s.config.Server.BaseURL,
	}, "layouts/main")
}

// handleDocs serves the API documentation page
// Shows API endpoints, usage examples, and system limits
func (s *Server) handleDocs(c *fiber.Ctx) error {
	retentionStats, err := utils.GenerateRetentionData(int64(s.config.Server.MaxUploadSize))
	if err != nil {
		s.logger.Error("failed to generate retention data", zap.Error(err))
		// Continue with empty retention data
	}

	return c.Render("docs", fiber.Map{
		"baseUrl": s.config.Server.BaseURL,
		"maxSize": humanize.IBytes(uint64(s.config.Server.MaxUploadSize)),
		"retention": fiber.Map{
			"noKey":   retentionStats.NoKeyRange,
			"withKey": retentionStats.WithKeyRange,
		},
		"apiKeyEnabled": s.hasMailer(),
	}, "layouts/main")
}

// Paste Creation Handlers

// handleUpload is a unified entry point for all upload types
// Automatically routes to the appropriate handler based on Content-Type and request format
// Supports:
// - multipart/form-data (file uploads)
// - application/json (JSON payload with content or URL)
// - any other Content-Type (treated as raw content)
func (s *Server) handleUpload(c *fiber.Ctx) error {
	// Get content type, removing any charset suffix
	contentType := strings.Split(c.Get("Content-Type"), ";")[0]

	switch contentType {
	case "multipart/form-data":
		// Check if we have a file in the form
		if _, err := c.FormFile("file"); err == nil {
			return s.handleMultipartUpload(c)
		}
		return fiber.NewError(fiber.StatusBadRequest, "No file provided in multipart form")

	case "application/json":
		// Verify we have a JSON body
		if len(c.Body()) == 0 {
			return fiber.NewError(fiber.StatusBadRequest, "Empty JSON body")
		}
		return s.handleJSONUpload(c)

	default:
		// Treat everything else as raw content
		if len(c.Body()) == 0 {
			return fiber.NewError(fiber.StatusBadRequest, "Empty content")
		}
		return s.handleRawUpload(c)
	}
}

// handleMultipartUpload processes multipart form file uploads
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

	// Get API key if present (make it optional)
	var apiKey *models.APIKey
	if key := c.Locals("apiKey"); key != nil {
		apiKey = key.(*models.APIKey)
	}

	// Create paste from raw content
	paste, err := s.createPasteFromRaw(c, content, &PasteOptions{
		Extension: extension,
		ExpiresIn: expiresIn,
		Private:   private,
		Filename:  filename,
		APIKey:    apiKey, // This can now be nil
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
// Returns: view count, last viewed time, and other metadata
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

// Paste Management Handlers

// handleListPastes returns a paginated list of pastes for the API key
// Optional query params:
//   - page: page number (default: 1)
//   - limit: items per page (default: 20)
//   - sort: sort order (default: "created_at desc")
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
// Verifies API key ownership before deletion
// Removes both storage content and database record
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

// handleRequestAPIKey handles the initial API key request
func (s *Server) handleRequestAPIKey(c *fiber.Ctx) error {
	// Check if email verification is available
	if !s.hasMailer() {
		return fiber.NewError(
			fiber.StatusServiceUnavailable,
			"Email verification is not available. Please contact the administrator.",
		)
	}

	ip := c.IP()
	if err := s.rateLimiter.Allow(fmt.Sprintf("api_key_request:%s", ip)); err != nil {
		return fiber.NewError(
			fiber.StatusTooManyRequests,
			"Please wait before requesting another API key",
		)
	}

	var req struct {
		Email string `json:"email" validate:"required,email"`
		Name  string `json:"name" validate:"required"`
	}

	if err := c.BodyParser(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "Invalid request body")
	}

	// Check if email is already in use
	var existingKey models.APIKey
	err := s.db.Where("email = ? AND verified = ?", req.Email, true).
		First(&existingKey).Error
	if err == nil {
		return fiber.NewError(
			fiber.StatusConflict,
			"An API key already exists for this email address",
		)
	}

	// Create API key with verification token
	apiKey := models.NewAPIKey()
	apiKey.Email = req.Email
	apiKey.Name = req.Name
	apiKey.VerifyToken = utils.GenerateID(64)
	apiKey.VerifyExpiry = time.Now().Add(24 * time.Hour)

	if err := s.db.Create(apiKey).Error; err != nil {
		s.logger.Error("failed to create API key",
			zap.String("email", req.Email),
			zap.Error(err))
		return fiber.NewError(
			fiber.StatusInternalServerError,
			"Failed to create API key",
		)
	}

	// Send verification email
	if err := s.mailer.SendVerification(req.Email, apiKey.VerifyToken); err != nil {
		s.logger.Error("failed to send verification email",
			zap.String("email", req.Email),
			zap.Error(err))

		// Delete the API key if we couldn't send the email
		s.db.Delete(apiKey)

		return fiber.NewError(
			fiber.StatusInternalServerError,
			"Failed to send verification email",
		)
	}

	return c.JSON(fiber.Map{
		"success": true,
		"message": "Please check your email to verify your API key",
	})
}

// handleVerifyAPIKey verifies the email and activates the API key
func (s *Server) handleVerifyAPIKey(c *fiber.Ctx) error {
	token := c.Params("token")

	var apiKey models.APIKey
	err := s.db.Where("verify_token = ? AND verify_expiry > ? AND verified = ?",
		token, time.Now(), false).First(&apiKey).Error

	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "Invalid or expired verification token")
	}

	// Activate the key
	apiKey.Verified = true
	apiKey.VerifyToken = ""
	if err := s.db.Save(&apiKey).Error; err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "Failed to verify API key")
	}

	return c.Render("verify_success", fiber.Map{
		"apiKey":  apiKey.Key,
		"baseUrl": s.config.Server.BaseURL,
	}, "layouts/main")
}

// Public Access Handlers

// handleView serves the content with syntax highlighting if applicable
// For URLs, redirects to target URL and increments view counter
// For text content, renders with syntax highlighting
// For other content types, redirects to download handler
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
// Sets appropriate content type and cache headers
// For text content, forces text/plain content type
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
// Sets Content-Disposition header for download
// Includes original filename in download prompt
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
// Removes both storage content and database record
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

// Helper Functions

// renderPasteView renders the paste view for text content
// Includes syntax highlighting using Chroma
// Supports language detection and line numbering
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

// getStorageSize returns total size of stored files in bytes
// Calculated as sum of all paste sizes in database
func (s *Server) getStorageSize() uint64 {
	var total uint64
	s.db.Model(&models.Paste{}).
		Select("COALESCE(SUM(size), 0)").
		Row().
		Scan(&total)
	return total
}

// addBaseURLToPasteResponse adds the configured base URL to all URL fields
// Modifies the response map in place, appending base URL to *_url fields
func (s *Server) addBaseURLToPasteResponse(response fiber.Map) {
	baseURL := strings.TrimSuffix(s.config.Server.BaseURL, "/")
	for key, value := range response {
		if strValue, ok := value.(string); ok {
			if strings.HasSuffix(key, "url") {
				response[key] = baseURL + strValue
			}
		}
	}
}
