package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pdmimpulse/LLM-Request-Balancer/internal/llm"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// DirectLLMRequestHandler processes LLM requests directly, bypassing the queue
// Used for debugging and testing purposes
type DirectLLMRequestHandler struct {
	// LLMService processes LLM requests
	LLMService *llm.Service
}

// NewDirectLLMRequestHandler creates a new direct handler
func NewDirectLLMRequestHandler(llmService *llm.Service) *DirectLLMRequestHandler {
	return &DirectLLMRequestHandler{
		LLMService: llmService,
	}
}

// ServeHTTP handles incoming HTTP requests for direct LLM processing without queueing
func (h *DirectLLMRequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Generate a request ID for tracking
	requestID := utils.GenerateShortID()
	ctx := utils.WithRequestID(r.Context(), requestID)

	utils.InfoContext(ctx, "Received new DIRECT request", map[string]interface{}{
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

	utils.InfoContext(ctx, "DIRECT request validated", map[string]interface{}{
		"provider": llmReq.ProviderName,
		"model":    llmReq.ModelName,
	})

	// Create an LLM request from the client request
	llmRequest := &llm.Request{
		ProviderName: llmReq.ProviderName,
		ModelName:    llmReq.ModelName,
		Payload:      llmReq.Payload,
	}

	// Create a context with a reasonable timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Process the request directly
	startTime := time.Now()
	fmt.Println("DIRECT: Starting direct processing of request", requestID)

	// Process the request directly through the LLM service
	response, err := h.LLMService.Process(timeoutCtx, llmRequest)
	if err != nil {
		utils.ErrorContext(ctx, err, "DIRECT: Error processing LLM request", map[string]interface{}{
			"provider": llmReq.ProviderName,
			"model":    llmReq.ModelName,
		})
		http.Error(w, fmt.Sprintf("Error from LLM provider: %v", err), http.StatusInternalServerError)
		return
	}

	// Log response details
	utils.InfoContext(ctx, "DIRECT: Response successful", map[string]interface{}{
		"provider":        response.ProviderName,
		"model":           response.ModelName,
		"tokens_used":     response.TokensUsed,
		"processing_time": time.Since(startTime).String(),
	})

	fmt.Println("DIRECT: Got successful response for request", requestID)

	// Set response headers
	w.Header().Set("X-Tokens-Used", fmt.Sprintf("%d", response.TokensUsed))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Processing-Time", time.Since(startTime).String())

	// Return the response
	w.WriteHeader(http.StatusOK)

	// Debug log the response result before sending
	if response.Result != nil {
		resultJSON, _ := json.Marshal(response.Result)
		utils.DebugContext(ctx, "DIRECT: Sending JSON response", map[string]interface{}{
			"response_bytes": len(resultJSON),
		})
	}

	fmt.Println("DIRECT: Encoding response to JSON")
	// Encode and send response
	if err := json.NewEncoder(w).Encode(response); err != nil {
		utils.ErrorContext(ctx, err, "DIRECT: Failed to encode response", nil)
		// At this point we've already written the status code, so we can't change it
		// Just log the error
	} else {
		utils.InfoContext(ctx, "DIRECT: Response sent to client", nil)
		fmt.Println("DIRECT: Response sent successfully")
	}
}
