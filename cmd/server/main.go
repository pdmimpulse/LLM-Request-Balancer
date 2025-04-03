// Package main is the entry point for the LLM Load Balancer service.
//
// This file implements the main application that ties together all components of the LLM Load Balancer:
// 1. Configuration loading from JSON files
// 2. Initialization of core components:
//   - Redis client for distributed state (optional)
//   - Rate limiters (Redis-based or in-memory) for token bucket implementation
//   - Request queues (Redis-based or in-memory) for handling overflow requests
//   - LLM service for communicating with external LLM providers
//   - Token calculator for estimating request costs
//   - Worker for processing queued requests
//
// 3. HTTP server setup with request handlers and health check endpoints
// 4. Graceful shutdown handling with proper cleanup of resources
//
// The service can be configured to use either Redis-based components for distributed
// deployments or in-memory components for simpler setups. It implements token bucket
// rate limiting to enforce per-model token limits and queues requests when limits are
// exceeded, processing them when tokens become available.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/config"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/handler"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/limiter"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/llm"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/queue"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// SimpleTokenCalculator provides a basic implementation of the TokenCalculator interface
type SimpleTokenCalculator struct{}

// CalculateTokenCost estimates the token cost for a request based on payload size
func (c *SimpleTokenCalculator) CalculateTokenCost(request *llm.Request) (int, error) {
	// This is a simplified implementation
	// In a real-world scenario, a more sophisticated token calculation would be used
	// based on the specific model and content

	// Default base cost
	baseCost := 10

	// Simple heuristic: estimate tokens based on payload complexity
	if request.Payload != nil {
		if content, ok := request.Payload["prompt"].(string); ok {
			// Rough estimate: 1 token per 4 characters
			return baseCost + len(content)/4, nil
		}
	}

	// Return default cost if we can't estimate
	return baseCost, nil
}

func main() {
	// Parse command line flags
	configPath := flag.String("config", "internal/config/config.json", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	logLevel := utils.LogLevel(cfg.Logging.Level)
	utils.InitLogger(logLevel, cfg.Logging.Pretty)

	// Create application context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling for graceful shutdown
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	// Initialize components based on configuration
	var redisClient redis.UniversalClient
	var rateLimiter limiter.Limiter
	var requestQueue queue.Queue

	// Initialize Redis client if needed
	if cfg.RateLimiter.UseRedis {
		redisOpts := &redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
			PoolSize: cfg.Redis.PoolSize,
		}
		redisClient = redis.NewClient(redisOpts)

		// Test connection
		_, err := redisClient.Ping(ctx).Result()
		if err != nil {
			utils.Error(err, "Failed to connect to Redis", nil)
			os.Exit(1)
		}

		utils.Info("Connected to Redis successfully", map[string]interface{}{
			"addr": cfg.Redis.Addr,
		})

		// Create Redis-based rate limiter
		limiterConfig := limiter.LimiterConfig{
			Type:            limiter.RedisLimiter,
			TokensPerSecond: cfg.RateLimiter.TokensPerSecond,
			BucketSize:      cfg.RateLimiter.BucketSize,
			RedisOptions: &limiter.RedisOptions{
				KeyPrefix: cfg.RateLimiter.KeyPrefix,
				KeyExpiry: 24 * time.Hour,
			},
		}
		redisLimiter, err := limiter.NewRedisTokenBucket(limiterConfig, redisClient)
		if err != nil {
			utils.Error(err, "Failed to create Redis rate limiter", nil)
			os.Exit(1)
		}
		rateLimiter = redisLimiter

		// Create Redis-based FIFO queue
		queueConfig := queue.QueueConfig{
			Type:    queue.RedisQueue,
			MaxSize: 10000, // Set an appropriate size based on expected load
			ItemTTL: 24 * time.Hour,
			RedisOptions: &queue.RedisOptions{
				KeyPrefix: "queue:",
				KeyExpiry: 72 * time.Hour,
			},
		}
		requestQueue, err = queue.NewQueue(queueConfig, redisClient)
		if err != nil {
			utils.Error(err, "Failed to create Redis queue", nil)
			os.Exit(1)
		}
	} else {
		// Create in-memory rate limiter
		limiterConfig := limiter.LimiterConfig{
			Type:            limiter.InMemoryLimiter,
			TokensPerSecond: cfg.RateLimiter.TokensPerSecond,
			BucketSize:      cfg.RateLimiter.BucketSize,
		}
		inMemoryLimiter, err := limiter.NewTokenBucket(limiterConfig)
		if err != nil {
			utils.Error(err, "Failed to create in-memory rate limiter", nil)
			os.Exit(1)
		}
		rateLimiter = inMemoryLimiter

		// Create in-memory FIFO queue
		queueConfig := queue.QueueConfig{
			Type:    queue.InMemoryQueue,
			MaxSize: 1000,
			ItemTTL: 1 * time.Hour,
		}
		requestQueue, err = queue.NewQueue(queueConfig, nil)
		if err != nil {
			utils.Error(err, "Failed to create in-memory queue", nil)
			os.Exit(1)
		}
	}

	// Initialize LLM service
	llmService, err := llm.NewService(&cfg.LLMProvider)
	if err != nil {
		utils.Error(err, "Failed to initialize LLM service", nil)
		os.Exit(1)
	}

	// Initialize token calculator
	tokenCalculator := &SimpleTokenCalculator{}

	// Create queue worker
	workerConfig := queue.WorkerConfig{
		PollingInterval: 100 * time.Millisecond, // Default values, adjust as needed
		MaxConcurrent:   10,
		RetryDelay:      500 * time.Millisecond,
	}
	worker := queue.NewWorker(requestQueue, rateLimiter, llmService, workerConfig)

	// Start worker
	worker.Start(ctx)
	defer worker.Stop()

	// Create HTTP handler
	llmHandler := handler.NewLLMRequestHandler(tokenCalculator, worker, llmService)

	// Create direct handler for debugging (no queue)
	directHandler := handler.NewDirectLLMRequestHandler(llmService)

	// Setup HTTP routes
	mux := http.NewServeMux()
	mux.Handle("/api/v1/completions", llmHandler)
	mux.Handle("/api/v1/direct/completions", directHandler) // Direct handler that skips the queue
	mux.HandleFunc("/health", llmHandler.HandleHealth)

	// Create HTTP server
	portStr := cfg.Server.Port
	if !strings.HasPrefix(portStr, ":") {
		portStr = ":" + portStr
	}

	server := &http.Server{
		Addr:         portStr,
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Start server in a goroutine
	go func() {
		utils.Info("Starting HTTP server", map[string]interface{}{
			"port": portStr,
		})

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			utils.Error(err, "HTTP server error", nil)
			cancel() // Cancel context to initiate shutdown
		}
	}()

	// Wait for termination signal
	<-signalCh
	utils.Info("Shutdown signal received, gracefully shutting down...", nil)

	// Create a context with timeout for shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()

	// Shutdown the HTTP server
	if err := server.Shutdown(shutdownCtx); err != nil {
		utils.Error(err, "Server shutdown error", nil)
	}

	// Wait for all background operations to complete
	cancel() // Cancel the application context

	utils.Info("Server shutdown complete", nil)
}
