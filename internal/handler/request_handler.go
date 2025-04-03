// Package handler provides HTTP handlers for the LLM Load Balancer.
//
// This file implements the core request handling logic for the LLM Load Balancer service.
// It's responsible for:
// 1. Receiving and validating incoming API requests for LLM processing
// 2. Calculating token costs for each request using the TokenCalculator
// 3. Managing request timeouts based on client preferences
// 4. Enqueueing requests to the worker queue for processing
// 5. Handling the asynchronous response flow from the worker
// 6. Providing health check endpoints for monitoring
//
// The handler acts as the bridge between the HTTP server and the internal
// processing components (token calculation, rate limiting, and request queuing).
// It ensures proper error handling and client communication throughout the
// request lifecycle.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pdmimpulse/LLM-Request-Balancer/internal/llm"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/queue"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// LLMRequestHandler processes incoming LLM API requests
type LLMRequestHandler struct {
	// TokenCalculator estimates token usage for requests
	TokenCalculator TokenCalculator

	// Worker handles queued requests
	Worker *queue.Worker

	// LLMService processes LLM requests
	LLMService *llm.Service

	// RequestTimeout is the maximum time to wait for a queued request
	RequestTimeout time.Duration
}

// TokenCalculator estimates the token cost for different request payloads
type TokenCalculator interface {
	// CalculateTokenCost estimates tokens needed for a given request
	CalculateTokenCost(request *llm.Request) (int, error)
}

// LLMRequest represents a client request to the load balancer
type LLMRequest struct {
	// ProviderName is the name of the LLM provider (e.g., "openai")
	ProviderName string `json:"provider"`

	// ModelName is the specific model to use (e.g., "gpt-4")
	ModelName string `json:"model"`

	// Payload contains the request data to send to the LLM
	Payload map[string]interface{} `json:"payload"`

	// MaxWaitTime optionally specifies how long to wait if the request is queued
	MaxWaitTime *time.Duration `json:"max_wait_time,omitempty"`
}

// NewLLMRequestHandler creates a new handler with the specified dependencies
func NewLLMRequestHandler(tokenCalculator TokenCalculator, worker *queue.Worker, llmService *llm.Service) *LLMRequestHandler {
	return &LLMRequestHandler{
		TokenCalculator: tokenCalculator,
		Worker:          worker,
		LLMService:      llmService,
		RequestTimeout:  5 * time.Minute, // Default timeout
	}
}

// ServeHTTP handles incoming HTTP requests for LLM processing
func (h *LLMRequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Generate a request ID for tracking
	requestID := utils.GenerateShortID()
	ctx := utils.WithRequestID(r.Context(), requestID)

	utils.InfoContext(ctx, "Received new request", map[string]interface{}{
		"method":     r.Method,
		"path":       r.URL.Path,
		"remote_ip":  r.RemoteAddr,
		"request_id": requestID,
	})

	// Only accept POST requests
	if r.Method != http.MethodPost {
		utils.WarnContext(ctx, "Method not allowed", map[string]interface{}{
			"method": r.Method,
		})
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read and log the request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		utils.ErrorContext(ctx, err, "Failed to read request body", nil)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Create a new reader for the body since we've consumed it
	r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// Log request body length (don't log the full body for privacy)
	utils.DebugContext(ctx, "Request body received", map[string]interface{}{
		"size_bytes": len(bodyBytes),
	})

	// Parse the request body
	var llmReq LLMRequest
	if err := json.NewDecoder(r.Body).Decode(&llmReq); err != nil {
		utils.ErrorContext(ctx, err, "Failed to decode request body", nil)
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	// Validate the request
	if llmReq.ProviderName == "" || llmReq.ModelName == "" || llmReq.Payload == nil {
		utils.WarnContext(ctx, "Missing required fields", map[string]interface{}{
			"provider":    llmReq.ProviderName,
			"model":       llmReq.ModelName,
			"has_payload": llmReq.Payload != nil,
		})
		http.Error(w, "Missing required fields: provider, model, or payload", http.StatusBadRequest)
		return
	}

	utils.InfoContext(ctx, "Request validated", map[string]interface{}{
		"provider": llmReq.ProviderName,
		"model":    llmReq.ModelName,
	})

	// Create an LLM request from the client request
	llmRequest := &llm.Request{
		ProviderName: llmReq.ProviderName,
		ModelName:    llmReq.ModelName,
		Payload:      llmReq.Payload,
	}

	// Calculate the token cost
	tokenCost, err := h.TokenCalculator.CalculateTokenCost(llmRequest)
	if err != nil {
		utils.ErrorContext(ctx, err, "Failed to calculate token cost", map[string]interface{}{
			"provider": llmReq.ProviderName,
			"model":    llmReq.ModelName,
		})
		http.Error(w, "Error calculating token cost", http.StatusInternalServerError)
		return
	}

	utils.InfoContext(ctx, "Token cost calculated", map[string]interface{}{
		"provider":   llmReq.ProviderName,
		"model":      llmReq.ModelName,
		"token_cost": tokenCost,
	})

	// Set custom request timeout if specified
	requestTimeout := h.RequestTimeout
	if llmReq.MaxWaitTime != nil && *llmReq.MaxWaitTime > 0 {
		// Cap the max wait time to the handler's maximum
		if *llmReq.MaxWaitTime < requestTimeout {
			requestTimeout = *llmReq.MaxWaitTime
		}
	}

	utils.DebugContext(ctx, "Request timeout set", map[string]interface{}{
		"timeout": requestTimeout.String(),
	})

	// Create a context with timeout for the request
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// Create a buffered channel for the response to prevent deadlocks
	responseCh := make(chan *llm.Response, 1)

	// Add request to the worker queue
	err = h.Worker.EnqueueRequest(reqCtx, llmRequest, tokenCost, responseCh)
	if err != nil {
		utils.ErrorContext(reqCtx, err, "Failed to enqueue request", map[string]interface{}{
			"provider":   llmReq.ProviderName,
			"model":      llmReq.ModelName,
			"token_cost": tokenCost,
		})
		http.Error(w, "Failed to process request", http.StatusInternalServerError)
		return
	}

	utils.InfoContext(reqCtx, "Request enqueued, waiting for processing", map[string]interface{}{
		"provider":   llmReq.ProviderName,
		"model":      llmReq.ModelName,
		"token_cost": tokenCost,
	})

	// Start a timer to measure response time
	startWait := time.Now()

	// Wait for response from the worker
	select {
	case resp := <-responseCh:
		waitTime := time.Since(startWait)
		utils.InfoContext(reqCtx, "Response received from worker", map[string]interface{}{
			"wait_time_ms": waitTime.Milliseconds(),
		})

		// Check if there was an error processing the request
		if resp == nil {
			utils.ErrorContext(reqCtx, fmt.Errorf("received nil response"), "Received nil response from worker", nil)
			http.Error(w, "Internal server error: nil response", http.StatusInternalServerError)
			return
		}

		if resp.Error != nil {
			utils.ErrorContext(reqCtx, resp.Error, "Error processing LLM request", map[string]interface{}{
				"provider": llmReq.ProviderName,
				"model":    llmReq.ModelName,
			})
			http.Error(w, fmt.Sprintf("Error from LLM provider: %v", resp.Error), http.StatusInternalServerError)
			return
		}

		// Log response details
		utils.InfoContext(reqCtx, "Response successful", map[string]interface{}{
			"provider":    resp.ProviderName,
			"model":       resp.ModelName,
			"tokens_used": resp.TokensUsed,
		})

		// Set any custom headers with metadata
		w.Header().Set("X-Tokens-Used", fmt.Sprintf("%d", resp.TokensUsed))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", requestID)

		// Return the response
		w.WriteHeader(http.StatusOK)

		// Debug log the response result before sending
		if resp.Result != nil {
			resultJSON, _ := json.Marshal(resp.Result)
			utils.DebugContext(reqCtx, "Sending JSON response", map[string]interface{}{
				"response_bytes": len(resultJSON),
			})
		}

		// Encode and send response
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			utils.ErrorContext(reqCtx, err, "Failed to encode response", nil)
			// At this point we've already written the status code, so we can't change it
			// Just log the error
		} else {
			utils.InfoContext(reqCtx, "Response sent to client", nil)
		}

	case <-reqCtx.Done():
		// Log the specific reason for timeout
		waitTime := time.Since(startWait)
		errMsg := "Request processing timed out"
		errDetails := map[string]interface{}{
			"provider":     llmReq.ProviderName,
			"model":        llmReq.ModelName,
			"token_cost":   tokenCost,
			"wait_time_ms": waitTime.Milliseconds(),
			"timeout_ms":   requestTimeout.Milliseconds(),
		}

		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			utils.WarnContext(r.Context(), errMsg+" due to deadline exceeded", errDetails)
		} else if errors.Is(reqCtx.Err(), context.Canceled) {
			utils.WarnContext(r.Context(), errMsg+" due to cancellation", errDetails)
		} else {
			utils.WarnContext(r.Context(), errMsg+": "+reqCtx.Err().Error(), errDetails)
		}

		http.Error(w, "Request timed out", http.StatusRequestTimeout)
		return
	}
}

// HandleHealth provides a health check endpoint
func (h *LLMRequestHandler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	// Simple health check - if this handler is running, the service is up
	// In a more comprehensive implementation, this would check dependencies too
	response := map[string]string{
		"status": "ok",
		"time":   time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
