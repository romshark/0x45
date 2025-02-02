package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/gabriel-vasile/mimetype"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/database"
	"github.com/watzon/0x45/internal/server/handlers"
	"github.com/watzon/0x45/internal/server/middleware"
	"github.com/watzon/0x45/internal/server/services"
	"github.com/watzon/0x45/internal/server/template"
	"github.com/watzon/0x45/internal/storage"
	"github.com/watzon/hdur"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type Server struct {
	app        *fiber.App
	db         *database.Database
	storage    *storage.StorageManager
	config     *config.Config
	logger     *zap.Logger
	services   *services.Services
	handlers   *handlers.Handlers
	middleware *middleware.Middleware
}

func New(config *config.Config, logger *zap.Logger) *Server {
	// Register markdown MIME types
	mimetype.Extend(func(raw []byte, limit uint32) bool {
		// Check for common markdown headers
		content := string(raw)
		if len(content) > 0 {
			firstLine := strings.Split(content, "\n")[0]
			if strings.HasPrefix(firstLine, "# ") || strings.HasPrefix(firstLine, "## ") {
				return true
			}
		}
		return false
	}, "text/markdown", ".md", ".markdown")

	// Custom parsers for fiber
	fiber.SetParserDecoder(fiber.ParserConfig{
		IgnoreUnknownKeys: true,
		ZeroEmpty:         true,
		ParserType: []fiber.ParserType{
			{
				Customtype: hdur.Duration{},
				Converter:  services.HdurDurationConverter,
			},
		},
	})

	// Initialize database
	db, err := database.New(config, &gorm.Config{
		// Logger: gormLogger,
	})
	if err != nil {
		logger.Fatal("Error connecting to database", zap.Error(err))
	}

	// Run migrations
	if err := db.Migrate(config); err != nil {
		logger.Fatal("Error running migrations", zap.Error(err))
	}

	// Initialize storage manager
	storageManager, err := storage.NewStorageManager(config)
	if err != nil {
		logger.Fatal("Failed to initialize storage", zap.Error(err))
	}

	// Initialize template engine with fallback support
	engine := template.New(config.Server.ViewsDirectory, "./views", ".hbs", logger)

	// Initialize services
	svc := services.NewServices(db.DB, logger, config)

	// Initialize middleware
	mw := middleware.NewMiddleware(db.DB, logger, config, svc)

	// Initialize handlers
	hdl := handlers.NewHandlers(db.DB, logger, config, svc)

	// Initialize Fiber app
	app := fiber.New(fiber.Config{
		ErrorHandler: errorHandler,
		BodyLimit:    int(config.Server.MaxUploadSize),
		Views:        engine,
		Prefork:      config.Server.Prefork,
		ServerHeader: config.Server.ServerHeader,
		AppName:      config.Server.AppName,
		ProxyHeader:  fiber.HeaderXForwardedFor,
	})

	// Add all middleware in the correct order
	for _, middleware := range mw.GetMiddleware() {
		app.Use(middleware)
	}

	// Serve static files
	app.Static("/public", config.Server.PublicDirectory)

	return &Server{
		app:        app,
		db:         db,
		storage:    storageManager,
		config:     config,
		logger:     logger,
		services:   svc,
		handlers:   hdl,
		middleware: mw,
	}
}

// SetupMiddleware configures all the middleware for the server
func (s *Server) SetupMiddleware() {
	// Add method override middleware
	s.app.Use(func(c *fiber.Ctx) error {
		// Check if this is a POST request with _method parameter
		if c.Method() == "POST" {
			method := c.FormValue("_method")
			if method != "" {
				c.Method(strings.ToUpper(method))
			}
		}
		return c.Next()
	})

	// Setup CORS
	s.app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
		AllowHeaders: "Origin, Content-Type, Accept",
	}))

	// Add request logging
	s.app.Use(logger.New(logger.Config{
		Format: "${time} ${ip} ${status} ${latency} ${method} ${path}\n",
	}))
}

// SetupRoutes configures all the routes for the server
func (s *Server) SetupRoutes() {
	// Setup middleware first
	s.SetupMiddleware()

	// Web interface routes
	s.app.Get("/", s.handlers.Web.HandleIndex)
	s.app.Get("/stats", s.handlers.Web.HandleStats)
	s.app.Get("/docs", s.handlers.Web.HandleDocs)
	s.app.Get("/submit", s.handlers.Web.HandleSubmit)

	// API Key routes
	keys := s.app.Group("/keys")
	keys.Post("/request", s.handlers.APIKey.HandleRequestAPIKey)
	keys.Get("/verify", s.handlers.APIKey.HandleVerifyAPIKey)

	// URL redirect route - must be before the group to avoid auth middleware
	s.app.Get("/u/:id", s.handlers.URL.HandleRedirect)

	// URL management routes
	urls := s.app.Group("/u")
	urls.Use(s.middleware.Auth.Auth(true))
	urls.Post("/", s.handlers.URL.HandleURLShorten)
	urls.Get("/list", s.handlers.URL.HandleListURLs)
	urls.Get("/:id/stats", s.handlers.URL.HandleURLStats)
	urls.Delete("/:id", s.handlers.URL.HandleDeleteURL)
	urls.Put("/:id/expiry", s.handlers.URL.HandleUpdateURLExpiration)

	// Paste routes - authenticated routes first
	pastes := s.app.Group("/p")
	pastes.Post("/", s.middleware.Auth.Auth(false), s.handlers.Paste.HandleUpload)
	pastes.Get("/list", s.middleware.Auth.Auth(true), s.handlers.Paste.HandleListPastes)
	pastes.Delete("/:id", s.middleware.Auth.Auth(false), s.handlers.Paste.HandleDeletePaste)
	pastes.Put("/:id/expiry", s.middleware.Auth.Auth(true), s.handlers.Paste.HandleUpdateExpiration)

	// Public paste routes - extension routes first (more specific)
	s.app.Get("/p/:id.:ext", func(c *fiber.Ctx) error {
		c.Locals("extension", c.Params("ext"))
		return s.handlers.Paste.HandleView(c)
	})
	s.app.Get("/p/:id/raw.:ext", func(c *fiber.Ctx) error {
		c.Locals("extension", c.Params("ext"))
		return s.handlers.Paste.HandleRawView(c)
	})
	s.app.Get("/p/:id/download.:ext", func(c *fiber.Ctx) error {
		c.Locals("extension", c.Params("ext"))
		return s.handlers.Paste.HandleDownload(c)
	})
	s.app.Get("/p/:id.:ext/image", func(c *fiber.Ctx) error {
		c.Locals("extension", c.Params("ext"))
		return s.handlers.Paste.HandleGetPasteImage(c)
	})

	// Non-extension paste routes last (more general)
	s.app.Get("/p/:id", s.handlers.Paste.HandleView)
	s.app.Get("/p/:id/raw", s.handlers.Paste.HandleRawView)
	s.app.Get("/p/:id/download", s.handlers.Paste.HandleDownload)
	s.app.Get("/p/:id/image", s.handlers.Paste.HandleGetPasteImage)
	s.app.Get("/p/:id/preview", s.handlers.Paste.HandlePreview)
	s.app.Delete("/p/:id/:key", s.handlers.Paste.HandleDeleteWithKey)
	s.app.Get("/p/:id/:key", s.handlers.Paste.HandleDeleteWithKey)
}

// Error handler
func errorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	message := "Internal Server Error"

	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
		message = e.Message
	}

	return c.Status(code).JSON(fiber.Map{
		"error": message,
	})
}

func (s *Server) Start(addr string) error {
	// Start cleanup scheduler
	if s.config.Server.Cleanup.Enabled {
		interval := fmt.Sprintf("%ds", s.config.Server.Cleanup.Interval)
		if err := s.services.StartCleanupScheduler(interval); err != nil {
			s.logger.Error("failed to start cleanup scheduler", zap.Error(err))
		}
	}

	// Setup routes
	s.SetupRoutes()

	// Start server
	return s.app.Listen(addr)
}

func (s *Server) GetApp() *fiber.App {
	return s.app
}

func (s *Server) GetDB() *database.Database {
	return s.db
}

func (s *Server) GetStorage() *storage.StorageManager {
	return s.storage
}

func (s *Server) GetConfig() *config.Config {
	return s.config
}

func (s *Server) GetLogger() *zap.Logger {
	return s.logger
}

func (s *Server) GetServices() *services.Services {
	return s.services
}

func (s *Server) GetHandlers() *handlers.Handlers {
	return s.handlers
}

func (s *Server) GetMiddleware() *middleware.Middleware {
	return s.middleware
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}

func (s *Server) Cleanup() error {
	if s.db != nil && s.db.DB != nil {
		if err := s.db.Close(); err != nil {
			s.logger.Error("failed to close database", zap.Error(err))
		}
	}
	return nil
}
