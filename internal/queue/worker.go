// Package queue provides queue functionality for the LLM Load Balancer.
//
// This file implements a worker that processes items from the FIFO queue,
// checking rate limits before forwarding requests to LLM providers.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pdmimpulse/LLM-Request-Balancer/internal/limiter"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/llm"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// WorkerConfig holds configuration for the queue worker
type WorkerConfig struct {
	// PollingInterval is how often to check the queue when empty
	PollingInterval time.Duration

	// MaxConcurrent is the maximum number of concurrent requests to process
	MaxConcurrent int

	// RetryDelay is how long to wait before retrying a rate-limited request
	RetryDelay time.Duration
}

// QueuedRequest represents a request waiting in the queue
type QueuedRequest struct {
	// Request is the LLM request to process
	Request *llm.Request `json:"request"`

	// TokenCost is the estimated token cost for rate limiting
	TokenCost int `json:"token_cost"`

	// RequestID is a unique identifier for tracking this request
	RequestID string `json:"request_id"`

	// CreatedAt is when the request was enqueued
	CreatedAt time.Time `json:"created_at"`
}

// Worker processes items from the queue, checking rate limits before processing
type Worker struct {
	// Queue to pull items from
	queue Queue

	// Rate limiter to check token availability
	rateLimiter limiter.Limiter

	// LLM service to process requests
	llmService *llm.Service

	// Configuration
	config WorkerConfig

	// Channel registry to store response channels
	channelRegistry *ChannelRegistry

	// Concurrency control
	semaphore chan struct{}
	wg        sync.WaitGroup

	// Control channels
	stopCh chan struct{}
	done   chan struct{}
}

// NewWorker creates a new queue worker
func NewWorker(queue Queue, rateLimiter limiter.Limiter, llmService *llm.Service, config WorkerConfig) *Worker {
	// Set defaults if not provided
	if config.PollingInterval <= 0 {
		config.PollingInterval = 100 * time.Millisecond
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 10
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = 500 * time.Millisecond
	}

	return &Worker{
		queue:           queue,
		rateLimiter:     rateLimiter,
		llmService:      llmService,
		config:          config,
		channelRegistry: NewChannelRegistry(),
		semaphore:       make(chan struct{}, config.MaxConcurrent),
		stopCh:          make(chan struct{}),
		done:            make(chan struct{}),
	}
}

// Start begins the worker processing loop
func (w *Worker) Start(ctx context.Context) {
	utils.Info("Starting queue worker", map[string]interface{}{
		"max_concurrent":   w.config.MaxConcurrent,
		"polling_interval": w.config.PollingInterval.String(),
	})

	go w.processLoop(ctx)
}

// Stop gracefully stops the worker
func (w *Worker) Stop() {
	utils.Info("Stopping queue worker", nil)
	close(w.stopCh)

	// Wait for processing to complete
	<-w.done
}

// EnqueueRequest adds a request to the queue
func (w *Worker) EnqueueRequest(ctx context.Context, request *llm.Request, tokenCost int, responseCh chan<- *llm.Response) error {
	// Create a queued request object with a unique request ID
	requestID := utils.GenerateShortID()

	queuedRequest := &QueuedRequest{
		Request:   request,
		TokenCost: tokenCost,
		RequestID: requestID,
		CreatedAt: time.Now(),
		// Note: ResponseChannel is not stored in the serialized item
	}

	// Register the response channel in our registry
	w.channelRegistry.Register(requestID, responseCh)

	// Serialize the request for the queue
	data, err := json.Marshal(queuedRequest)
	if err != nil {
		w.channelRegistry.Remove(requestID) // Clean up on error
		return fmt.Errorf("failed to serialize request: %w", err)
	}

	// Create a queue item
	item := &QueueItem{
		ID:          requestID,
		Data:        data,
		Priority:    0, // Default priority
		EnqueueTime: time.Now(),
		Metadata: map[string]interface{}{
			"provider":   request.ProviderName,
			"model":      request.ModelName,
			"tokens":     tokenCost,
			"request_id": requestID, // Store request ID in metadata for easier access
		},
	}

	// Enqueue the item
	if err := w.queue.Enqueue(ctx, item); err != nil {
		w.channelRegistry.Remove(requestID) // Clean up on error
		return fmt.Errorf("failed to enqueue request: %w", err)
	}

	utils.InfoContext(ctx, "Request enqueued", map[string]interface{}{
		"request_id": requestID,
		"provider":   request.ProviderName,
		"model":      request.ModelName,
		"token_cost": tokenCost,
	})

	return nil
}

// processLoop continuously processes items from the queue
func (w *Worker) processLoop(ctx context.Context) {
	defer close(w.done)

	for {
		select {
		case <-ctx.Done():
			utils.Info("Context canceled, stopping worker", nil)
			w.waitForPendingJobs()
			return
		case <-w.stopCh:
			utils.Info("Stop signal received, stopping worker", nil)
			w.waitForPendingJobs()
			return
		default:
			// Continue processing
		}

		// Try to acquire a semaphore slot
		select {
		case w.semaphore <- struct{}{}:
			// We got a slot, dequeue an item
			item, err := w.queue.Dequeue(ctx)
			if err != nil {
				if !errors.Is(err, ErrQueueEmpty) {
					utils.ErrorContext(ctx, err, "Failed to dequeue item", nil)
				}
				<-w.semaphore // Release the slot
				time.Sleep(w.config.PollingInterval)
				continue
			}

			// Process the item in a separate goroutine
			w.wg.Add(1)
			go w.processItem(ctx, item)
		default:
			// No slots available, wait a bit
			time.Sleep(w.config.PollingInterval)
		}
	}
}

// waitForPendingJobs waits for all in-progress jobs to complete
func (w *Worker) waitForPendingJobs() {
	utils.Info("Waiting for pending jobs to complete", nil)
	w.wg.Wait()
	utils.Info("All pending jobs completed", nil)
}

// processItem processes a single queue item
func (w *Worker) processItem(ctx context.Context, item *QueueItem) {
	defer w.wg.Done()
	defer func() { <-w.semaphore }() // Release the semaphore slot

	// Create a context with timeout
	itemCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	fmt.Printf("WORKER: Processing item %s\n", item.ID)

	// Deserialize the queued request
	var queuedRequest QueuedRequest
	if err := json.Unmarshal(item.Data, &queuedRequest); err != nil {
		fmt.Printf("WORKER ERROR: Failed to deserialize request: %v\n", err)
		utils.ErrorContext(itemCtx, err, "Failed to deserialize queued request", map[string]interface{}{
			"item_id": item.ID,
		})
		return
	}

	fmt.Printf("WORKER: Deserialized request %s (provider=%s, model=%s)\n",
		queuedRequest.RequestID, queuedRequest.Request.ProviderName, queuedRequest.Request.ModelName)

	// Get the response channel from the registry
	responseCh := w.channelRegistry.Get(queuedRequest.RequestID)
	if responseCh == nil {
		fmt.Printf("WORKER WARNING: No response channel found for request %s\n", queuedRequest.RequestID)
		utils.WarnContext(itemCtx, "No response channel found in registry", map[string]interface{}{
			"request_id": queuedRequest.RequestID,
		})
		// Continue processing anyway in case we want to log the result
	}

	// Check if rate limit allows this request
	allowed, err := w.rateLimiter.Allow(itemCtx, queuedRequest.TokenCost)
	if err != nil {
		fmt.Printf("WORKER ERROR: Rate limiter error: %v\n", err)
		utils.ErrorContext(itemCtx, err, "Rate limiter error", map[string]interface{}{
			"request_id": queuedRequest.RequestID,
			"token_cost": queuedRequest.TokenCost,
		})

		// Send error to response channel if available
		if responseCh != nil {
			fmt.Printf("WORKER: Sending rate limiter error to response channel\n")
			responseCh <- &llm.Response{
				ProviderName: queuedRequest.Request.ProviderName,
				ModelName:    queuedRequest.Request.ModelName,
				Error:        err,
			}
			// Remove channel from registry after use
			w.channelRegistry.Remove(queuedRequest.RequestID)
		}
		return
	}

	if !allowed {
		// Not enough tokens available, re-enqueue with delay
		fmt.Printf("WORKER: Rate limited, will retry after delay\n")
		utils.InfoContext(itemCtx, "Rate limited, will retry", map[string]interface{}{
			"request_id":  queuedRequest.RequestID,
			"token_cost":  queuedRequest.TokenCost,
			"retry_delay": w.config.RetryDelay.String(),
		})

		// Re-enqueue with a delay
		delayedItem := &QueueItem{
			ID:           item.ID,
			Data:         item.Data,
			Priority:     item.Priority,
			EnqueueTime:  time.Now(),
			ProcessAfter: time.Now().Add(w.config.RetryDelay),
			Metadata:     item.Metadata,
		}

		if err := w.queue.Enqueue(itemCtx, delayedItem); err != nil {
			fmt.Printf("WORKER ERROR: Failed to re-enqueue rate-limited request: %v\n", err)
			utils.ErrorContext(itemCtx, err, "Failed to re-enqueue rate-limited request", map[string]interface{}{
				"request_id": queuedRequest.RequestID,
			})

			// Send error to response channel if available
			if responseCh != nil {
				fmt.Printf("WORKER: Sending re-enqueue error to response channel\n")
				responseCh <- &llm.Response{
					ProviderName: queuedRequest.Request.ProviderName,
					ModelName:    queuedRequest.Request.ModelName,
					Error:        err,
				}
				// Remove channel from registry after use
				w.channelRegistry.Remove(queuedRequest.RequestID)
			}
		}
		return
	}

	// Process the request via LLM service
	fmt.Printf("WORKER: Starting LLM processing for request %s\n", queuedRequest.RequestID)
	utils.InfoContext(itemCtx, "Processing request", map[string]interface{}{
		"request_id": queuedRequest.RequestID,
		"provider":   queuedRequest.Request.ProviderName,
		"model":      queuedRequest.Request.ModelName,
		"queue_time": time.Since(queuedRequest.CreatedAt).String(),
	})

	processStart := time.Now()
	response, err := w.llmService.Process(itemCtx, queuedRequest.Request)
	fmt.Printf("WORKER: LLM processing completed in %v\n", time.Since(processStart))

	if err != nil {
		fmt.Printf("WORKER ERROR: Failed to process request: %v\n", err)
		utils.ErrorContext(itemCtx, err, "Failed to process request", map[string]interface{}{
			"request_id": queuedRequest.RequestID,
		})

		// Send error to response channel if available
		if responseCh != nil {
			fmt.Printf("WORKER: Sending error response to channel\n")
			responseCh <- &llm.Response{
				ProviderName: queuedRequest.Request.ProviderName,
				ModelName:    queuedRequest.Request.ModelName,
				Error:        err,
			}
			// Remove channel from registry after use
			w.channelRegistry.Remove(queuedRequest.RequestID)
		}
		return
	}

	// Send response to channel if available
	if responseCh != nil {
		fmt.Printf("WORKER: Sending successful response to channel (tokens=%d)\n", response.TokensUsed)
		utils.InfoContext(itemCtx, "Sending response", map[string]interface{}{
			"request_id":  queuedRequest.RequestID,
			"tokens_used": response.TokensUsed,
			"total_time":  time.Since(queuedRequest.CreatedAt).String(),
		})

		// First check if response is nil
		if response == nil {
			fmt.Printf("WORKER ERROR: Response is nil!\n")
			responseCh <- &llm.Response{
				ProviderName: queuedRequest.Request.ProviderName,
				ModelName:    queuedRequest.Request.ModelName,
				Error:        fmt.Errorf("null response from LLM service"),
			}
			// Remove channel from registry after use
			w.channelRegistry.Remove(queuedRequest.RequestID)
			return
		}

		// Log response content
		if response.Result != nil {
			resultLen := 0
			resultJSON, _ := json.Marshal(response.Result)
			if resultJSON != nil {
				resultLen = len(resultJSON)
			}
			fmt.Printf("WORKER: Response result size: %d bytes\n", resultLen)
		}

		// Send response on channel
		fmt.Printf("WORKER: Sending response on channel\n")
		responseCh <- response
		fmt.Printf("WORKER: Response sent on channel\n")

		// Remove channel from registry after use
		w.channelRegistry.Remove(queuedRequest.RequestID)
	} else {
		fmt.Printf("WORKER WARNING: No response channel available\n")
		utils.WarnContext(itemCtx, "No response channel available", map[string]interface{}{
			"request_id": queuedRequest.RequestID,
		})
	}

	fmt.Printf("WORKER: Done processing item %s\n", item.ID)
}
