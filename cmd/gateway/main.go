// Package main is the entry point for the Fabric Payment Gateway
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/fabric-payment-gateway/internal/api"
	"github.com/fabric-payment-gateway/internal/config"
	"github.com/fabric-payment-gateway/internal/fabric"
)

var (
	// Version is set at build time
	Version   = "1.0.0"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

func main() {
	// Parse command line flags
	configPath := flag.String("config", "configs/config.yaml", "Path to configuration file")
	showVersion := flag.Bool("version", false, "Show version information")
	flag.Parse()

	if *showVersion {
		fmt.Printf("Fabric Payment Gateway\n")
		fmt.Printf("Version:    %s\n", Version)
		fmt.Printf("Build Time: %s\n", BuildTime)
		fmt.Printf("Git Commit: %s\n", GitCommit)
		os.Exit(0)
	}

	// Initialize logger
	logger, err := initLogger("info", "json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("Starting Fabric Payment Gateway",
		zap.String("version", Version),
		zap.String("configPath", *configPath),
	)

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Reinitialize logger with config settings
	logger, err = initLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		logger.Fatal("Failed to reinitialize logger", zap.Error(err))
	}

	logger.Info("Configuration loaded successfully",
		zap.String("mspID", cfg.Fabric.MSPID),
		zap.String("channel", cfg.Fabric.ChannelName),
		zap.String("chaincode", cfg.Fabric.Chaincode.Name),
	)

	// Initialize Fabric client
	fabricClient := fabric.NewClient(&cfg.Fabric, logger)

	// Connect to Fabric network
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Fabric.Timeouts.Connect)
	if err := fabricClient.Connect(ctx); err != nil {
		logger.Warn("Failed to connect to Fabric network on startup",
			zap.Error(err),
			zap.String("note", "Will retry on first request"),
		)
	}
	cancel()

	// Initialize API handler
	handler, err := api.NewHandler(cfg, fabricClient, logger)
	if err != nil {
		logger.Fatal("Failed to initialize API handler", zap.Error(err))
	}

	// Setup Gin router
	if cfg.Logging.Level != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()

	// Add middleware
	router.Use(api.RecoveryMiddleware(logger))
	router.Use(api.LoggingMiddleware(logger))
	router.Use(api.RequestIDMiddleware())
	router.Use(api.SecurityHeadersMiddleware())
	router.Use(api.APIKeyMiddleware(cfg.APIKey))

	// Add rate limiter
	rateLimiter := api.NewRateLimiter(cfg.Transaction.RateLimit, cfg.Transaction.RateLimitWindow)
	router.Use(rateLimiter.RateLimitMiddleware())

	if cfg.Audit.Enabled {
		router.Use(api.AuditMiddleware(logger, cfg.Audit.MaskFields))
	}

	// Register routes
	handler.RegisterRoutes(router)

	// Create HTTP server
	server := &http.Server{
		Addr:         cfg.GetAddress(),
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Configure mTLS if enabled
	if cfg.Server.MTLS.Enabled {
		tlsConfig, err := configureMTLS(cfg.Server.MTLS)
		if err != nil {
			logger.Fatal("Failed to configure mTLS", zap.Error(err))
		}
		server.TLSConfig = tlsConfig
		logger.Info("mTLS enabled for client authentication")
	}

	// Start server in goroutine
	go func() {
		logger.Info("HTTP server starting",
			zap.String("address", cfg.GetAddress()),
			zap.Bool("mtls", cfg.Server.MTLS.Enabled),
		)

		var err error
		if cfg.Server.MTLS.Enabled {
			err = server.ListenAndServeTLS(
				cfg.Server.MTLS.CertFile,
				cfg.Server.MTLS.KeyFile,
			)
		} else {
			err = server.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutdown signal received, gracefully shutting down...")

	// Graceful shutdown
	ctx, cancel = context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Server shutdown error", zap.Error(err))
	}

	// Close Fabric connection
	fabricClient.Close()

	logger.Info("Server shutdown complete")
}

// initLogger initializes the zap logger
func initLogger(level, format string) (*zap.Logger, error) {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	var cfg zap.Config
	if format == "json" {
		cfg = zap.NewProductionConfig()
	} else {
		cfg = zap.NewDevelopmentConfig()
	}
	cfg.Level = zap.NewAtomicLevelAt(zapLevel)

	return cfg.Build()
}

// configureMTLS configures mutual TLS for client authentication
func configureMTLS(cfg config.MTLSConfig) (*tls.Config, error) {
	// Load CA certificate for client verification
	caCert, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to add CA certificate to pool")
	}

	return &tls.Config{
		ClientCAs:  caCertPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
	}, nil
}
