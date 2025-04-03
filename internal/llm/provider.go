// Package llm provides interfaces and implementations for interacting with various LLM provider APIs.
//
// This file handles:
// - Abstract interfaces for LLM providers
// - Implementation for different providers (OpenAI, Anthropic, etc.)
// - Asynchronous request handling
// - Request/response mapping
// - Token counting and cost estimation
//
// TODO: Implement token counting utilities for different models
//   - Create TokenCounter interface (lines ~50-55 in provider.go)
//   - Implement model-specific counters for OpenAI and Anthropic (lines ~150-200 in provider.go)
//   - Add token cost functions referenced in config.json (internal/utils/token_utils.go)
//
// TODO: Add support for streaming responses
//   - Update Provider interface to include streaming methods (lines ~35-40 in provider.go)
//   - Implement streaming handlers for each provider (lines ~250-300 in provider.go)
//
// TODO: Implement retry logic with exponential backoff
//   - Add RetryPolicy struct with configurable parameters (lines ~60-70 in provider.go)
//   - Integrate with http client wrapper (lines ~100-120 in provider.go)
//   - Use retry settings from config.LLMProviderConfig (internal/config/config.go)
//
// TODO: Add comprehensive error handling and logging
//   - Create error types for different failure scenarios (lines ~25-30 in provider.go)
//   - Add structured logging throughout request lifecycle (lines ~80-90, ~130-140 in provider.go)
//   - Use logger from internal/utils/logger.go
//
// TODO: Implement provider-specific request/response mapping
//   - Create mapping functions for each provider (lines ~210-240 in provider.go)
//   - Add validation for required fields (lines ~220-230 in provider.go)
//
// TODO: Add more providers
//   - Implement Groq provider (lines ~350-400 in provider.go)
//   - Add Groq configuration in config.json (internal/config/config.json)
//   - Create Groq-specific request/response mapping (lines ~410-430 in provider.go)
//   - Implement token counting for Groq models (internal/utils/token_utils.go)

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/pdmimpulse/LLM-Request-Balancer/internal/config"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// Common errors
var (
	ErrProviderNotFound = errors.New("provider not found")
	ErrModelNotFound    = errors.New("model not found")
	ErrRequestFailed    = errors.New("request failed")
	ErrContextCanceled  = errors.New("context canceled")
)

// Request represents a generic LLM request
type Request struct {
	ProviderName string                 `json:"provider_name"`
	ModelName    string                 `json:"model_name"`
	Payload      map[string]interface{} `json:"payload"`
}

// Response represents a generic LLM response
type Response struct {
	ProviderName string                 `json:"provider_name"`
	ModelName    string                 `json:"model_name"`
	Result       map[string]interface{} `json:"result"`
	TokensUsed   int                    `json:"tokens_used"`
	Error        error                  `json:"-"`
}

// Provider defines the interface for an LLM provider
type Provider interface {
	// Name returns the provider name
	Name() string

	// SupportedModels returns the list of supported models
	SupportedModels() []string

	// EstimateTokens estimates the number of tokens in a request
	EstimateTokens(req *Request) (int, error)

	// ProcessRequest sends a request to the provider API
	ProcessRequest(ctx context.Context, req *Request) (*Response, error)

	// ProcessRequestAsync sends a request to the provider API asynchronously
	ProcessRequestAsync(ctx context.Context, req *Request, responseCh chan<- *Response)
}

// Service manages multiple LLM providers
type Service struct {
	providers       map[string]Provider
	defaultProvider string
	defaultModel    string
	httpClient      *http.Client
	mu              sync.RWMutex
}

// NewService creates a new LLM service with the given configuration
func NewService(cfg *config.LLMProviderConfig) (*Service, error) {
	if cfg.DefaultProvider == "" {
		return nil, errors.New("default provider must be specified")
	}
	if cfg.DefaultModel == "" {
		return nil, errors.New("default model must be specified")
	}

	// Create HTTP client with appropriate timeouts
	httpClient := &http.Client{
		Timeout: cfg.Providers[cfg.DefaultProvider].RequestTimeout,
	}

	service := &Service{
		providers:       make(map[string]Provider),
		defaultProvider: cfg.DefaultProvider,
		defaultModel:    cfg.DefaultModel,
		httpClient:      httpClient,
	}

	// Initialize providers based on config
	for name, providerCfg := range cfg.Providers {
		var provider Provider
		var err error

		switch name {
		case "openai":
			provider, err = newOpenAIProvider(name, providerCfg, httpClient)
		case "anthropic":
			provider, err = newAnthropicProvider(name, providerCfg, httpClient)
		// Add other providers as needed
		default:
			provider, err = newGenericProvider(name, providerCfg, httpClient)
		}

		if err != nil {
			return nil, fmt.Errorf("failed to initialize provider %s: %w", name, err)
		}

		service.providers[name] = provider
	}

	utils.Info("LLM service initialized", map[string]interface{}{
		"providers":       service.GetProviderNames(),
		"defaultProvider": service.defaultProvider,
		"defaultModel":    service.defaultModel,
	})

	return service, nil
}

// GetProviderNames returns the names of all registered providers
func (s *Service) GetProviderNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.providers))
	for name := range s.providers {
		names = append(names, name)
	}
	return names
}

// GetProvider returns a provider by name
func (s *Service) GetProvider(name string) (Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	provider, ok := s.providers[name]
	if !ok {
		return nil, ErrProviderNotFound
	}
	return provider, nil
}

// Process sends an LLM request to the appropriate provider
func (s *Service) Process(ctx context.Context, req *Request) (*Response, error) {
	// Use default provider and model if not specified
	if req.ProviderName == "" {
		req.ProviderName = s.defaultProvider
	}
	if req.ModelName == "" {
		req.ModelName = s.defaultModel
	}

	// Get the appropriate provider
	provider, err := s.GetProvider(req.ProviderName)
	if err != nil {
		return nil, err
	}

	// Start a timer to measure processing time
	timer := utils.NewTimer(fmt.Sprintf("LLM request to %s/%s", req.ProviderName, req.ModelName))
	defer timer.Stop()

	// Process the request
	return provider.ProcessRequest(ctx, req)
}

// ProcessAsync sends an LLM request asynchronously
func (s *Service) ProcessAsync(ctx context.Context, req *Request, responseCh chan<- *Response) {
	// Use default provider and model if not specified
	if req.ProviderName == "" {
		req.ProviderName = s.defaultProvider
	}
	if req.ModelName == "" {
		req.ModelName = s.defaultModel
	}

	// Get the appropriate provider
	provider, err := s.GetProvider(req.ProviderName)
	if err != nil {
		responseCh <- &Response{
			ProviderName: req.ProviderName,
			ModelName:    req.ModelName,
			Error:        err,
		}
		return
	}

	// Process the request asynchronously
	provider.ProcessRequestAsync(ctx, req, responseCh)
}

// EstimateTokens estimates the number of tokens in a request
func (s *Service) EstimateTokens(req *Request) (int, error) {
	// Use default provider and model if not specified
	if req.ProviderName == "" {
		req.ProviderName = s.defaultProvider
	}
	if req.ModelName == "" {
		req.ModelName = s.defaultModel
	}

	// Get the appropriate provider
	provider, err := s.GetProvider(req.ProviderName)
	if err != nil {
		return 0, err
	}

	// Estimate tokens
	return provider.EstimateTokens(req)
}

// BaseProvider provides common functionality for all providers
type BaseProvider struct {
	name        string
	config      config.ProviderSettings
	models      map[string]bool
	httpClient  *http.Client
	retryPolicy *retryPolicy
}

// retryPolicy defines how retries are handled
type retryPolicy struct {
	maxRetries int
	minBackoff time.Duration
	maxBackoff time.Duration
}

// newBaseProvider creates a new base provider
func newBaseProvider(name string, cfg config.ProviderSettings, httpClient *http.Client) BaseProvider {
	models := make(map[string]bool)
	for _, model := range cfg.Models {
		models[model.Name] = true
	}

	return BaseProvider{
		name:       name,
		config:     cfg,
		models:     models,
		httpClient: httpClient,
		retryPolicy: &retryPolicy{
			maxRetries: cfg.MaxRetries,
			minBackoff: cfg.RetryBackoffMin,
			maxBackoff: cfg.RetryBackoffMax,
		},
	}
}

// Name returns the provider name
func (p *BaseProvider) Name() string {
	return p.name
}

// SupportedModels returns the list of supported models
func (p *BaseProvider) SupportedModels() []string {
	models := make([]string, 0, len(p.models))
	for model := range p.models {
		models = append(models, model)
	}
	return models
}

// doRequest performs an HTTP request with retries
func (p *BaseProvider) doRequest(ctx context.Context, method, url string, body []byte, headers map[string]string) ([]byte, error) {
	var resp *http.Response
	var respBody []byte

	// Retry logic
	for attempt := 0; attempt <= p.retryPolicy.maxRetries; attempt++ {
		// Check if context is canceled
		if ctx.Err() != nil {
			return nil, ErrContextCanceled
		}

		// Create request
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		// Add headers
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		// Make the request
		resp, err = p.httpClient.Do(req)
		if err != nil {
			utils.WarnContext(ctx, "Request failed, will retry", map[string]interface{}{
				"attempt": attempt,
				"error":   err.Error(),
				"url":     url,
			})

			// Wait before retrying
			if attempt < p.retryPolicy.maxRetries {
				backoff := calculateBackoff(attempt, p.retryPolicy.minBackoff, p.retryPolicy.maxBackoff)
				time.Sleep(backoff)
				continue
			}
			return nil, fmt.Errorf("%w: %v", ErrRequestFailed, err)
		}

		// Read response body
		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			utils.WarnContext(ctx, "Failed to read response body, will retry", map[string]interface{}{
				"attempt": attempt,
				"error":   err.Error(),
				"url":     url,
			})

			if attempt < p.retryPolicy.maxRetries {
				backoff := calculateBackoff(attempt, p.retryPolicy.minBackoff, p.retryPolicy.maxBackoff)
				time.Sleep(backoff)
				continue
			}
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}

		// Check response status
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// Success
			return respBody, nil
		}

		if attempt < p.retryPolicy.maxRetries && isRetryableStatusCode(resp.StatusCode) {
			utils.WarnContext(ctx, "Received retryable status code", map[string]interface{}{
				"attempt":    attempt,
				"statusCode": resp.StatusCode,
				"url":        url,
			})
			backoff := calculateBackoff(attempt, p.retryPolicy.minBackoff, p.retryPolicy.maxBackoff)
			time.Sleep(backoff)
			continue
		}

		// Non-retryable error
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, respBody)
	}

	return respBody, nil
}

// calculateBackoff calculates exponential backoff with jitter
func calculateBackoff(attempt int, minBackoff, maxBackoff time.Duration) time.Duration {
	backoff := minBackoff * (1 << uint(attempt))
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	// Add jitter (±20%)
	jitter := time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64()))
	return jitter
}

// isRetryableStatusCode determines if a status code should be retried
func isRetryableStatusCode(statusCode int) bool {
	return statusCode == 429 || // Too Many Requests
		statusCode == 500 || // Internal Server Error
		statusCode == 502 || // Bad Gateway
		statusCode == 503 || // Service Unavailable
		statusCode == 504 // Gateway Timeout
}

// Now let's implement specific providers

// OpenAIProvider implements the Provider interface for OpenAI
type OpenAIProvider struct {
	BaseProvider
}

// newOpenAIProvider creates a new OpenAI provider
func newOpenAIProvider(name string, cfg config.ProviderSettings, httpClient *http.Client) (*OpenAIProvider, error) {
	base := newBaseProvider(name, cfg, httpClient)
	return &OpenAIProvider{
		BaseProvider: base,
	}, nil
}

// EstimateTokens estimates the number of tokens in an OpenAI request
func (p *OpenAIProvider) EstimateTokens(req *Request) (int, error) {
	// Simple estimation based on request content
	inputText := ""

	// Handle messages array - the issue is in the type assertions
	if messagesRaw, ok := req.Payload["messages"]; ok {
		messages, ok := messagesRaw.([]interface{})
		if !ok {
			// Try alternative formats that might occur in tests
			if msgsArray, ok := messagesRaw.([]map[string]interface{}); ok {
				for _, msg := range msgsArray {
					if content, ok := msg["content"].(string); ok {
						inputText += content + " "
					}
				}
			}
		} else {
			// Normal case: messages is []interface{}
			for _, msgRaw := range messages {
				msg, ok := msgRaw.(map[string]interface{})
				if ok {
					if content, ok := msg["content"].(string); ok {
						inputText += content + " "
					}
				}
			}
		}
	} else if prompt, ok := req.Payload["prompt"].(string); ok {
		inputText = prompt
	}

	// Log the input for debugging
	fmt.Printf("Estimating tokens for text: '%s'\n", inputText)

	// If we have no text, return at least 1 token (system overhead)
	if len(inputText) == 0 {
		return 1, nil
	}

	// Very rough estimation - 4 characters per token
	return len(inputText) / 4, nil
}

// ProcessRequest sends a request to the OpenAI API
func (p *OpenAIProvider) ProcessRequest(ctx context.Context, req *Request) (*Response, error) {
	// Debug direct to console
	fmt.Println("OpenAI Provider: Starting request processing")
	requestID := "unknown"
	if reqID, ok := ctx.Value("request_id").(string); ok {
		requestID = reqID
	}
	fmt.Printf("OpenAI Provider: Request ID %s\n", requestID)

	// Check if model is supported
	if !p.models[req.ModelName] {
		fmt.Printf("OpenAI Provider: Model %s not found\n", req.ModelName)
		return nil, ErrModelNotFound
	}

	// Create a copy of the payload to avoid modifying the original
	payload := make(map[string]interface{})
	for k, v := range req.Payload {
		payload[k] = v
	}

	// Add model to payload - this is required by OpenAI
	payload["model"] = req.ModelName

	// Prepare request body
	reqBody, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("OpenAI Provider: Error marshaling request: %v\n", err)
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	fmt.Printf("OpenAI Provider: Request body size: %d bytes\n", len(reqBody))

	// Prepare request headers
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + p.config.APIKey,
	}

	// Determine endpoint based on payload
	endpoint := p.config.BaseURL
	endpoint += "/chat/completions"

	fmt.Printf("OpenAI Provider: Using endpoint %s\n", endpoint)

	// Send request
	fmt.Println("OpenAI Provider: Sending request to OpenAI")
	requestStart := time.Now()
	respBody, err := p.doRequest(ctx, "POST", endpoint, reqBody, headers)
	if err != nil {
		fmt.Printf("OpenAI Provider: Error from OpenAI API: %v\n", err)
		return nil, err
	}
	fmt.Printf("OpenAI Provider: Received response from OpenAI after %v\n", time.Since(requestStart))
	fmt.Printf("OpenAI Provider: Response size: %d bytes\n", len(respBody))

	// Parse response
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Printf("OpenAI Provider: Error unmarshal response: %v\n", err)
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	fmt.Println("OpenAI Provider: Successfully parsed response JSON")

	// Extract token usage
	tokensUsed := 0
	if usage, ok := result["usage"].(map[string]interface{}); ok {
		if total, ok := usage["total_tokens"].(float64); ok {
			tokensUsed = int(total)
			fmt.Printf("OpenAI Provider: Tokens used: %d\n", tokensUsed)
		}
	}

	// Create response
	response := &Response{
		ProviderName: p.name,
		ModelName:    req.ModelName,
		Result:       result,
		TokensUsed:   tokensUsed,
	}

	// Log response creation
	fmt.Println("OpenAI Provider: Created response object, returning to caller")

	return response, nil
}

// ProcessRequestAsync sends a request to the OpenAI API asynchronously
func (p *OpenAIProvider) ProcessRequestAsync(ctx context.Context, req *Request, responseCh chan<- *Response) {
	go func() {
		resp, err := p.ProcessRequest(ctx, req)
		if err != nil {
			responseCh <- &Response{
				ProviderName: p.name,
				ModelName:    req.ModelName,
				Error:        err,
			}
			return
		}
		responseCh <- resp
	}()
}

// AnthropicProvider implements the Provider interface for Anthropic
type AnthropicProvider struct {
	BaseProvider
}

// newAnthropicProvider creates a new Anthropic provider
func newAnthropicProvider(name string, cfg config.ProviderSettings, httpClient *http.Client) (*AnthropicProvider, error) {
	base := newBaseProvider(name, cfg, httpClient)
	return &AnthropicProvider{
		BaseProvider: base,
	}, nil
}

// EstimateTokens estimates the number of tokens in an Anthropic request
func (p *AnthropicProvider) EstimateTokens(req *Request) (int, error) {
	// Simple estimation for Anthropic (similar to OpenAI but might have different tokenization)
	inputText := ""

	if prompt, ok := req.Payload["prompt"].(string); ok {
		inputText = prompt
	} else if messages, ok := req.Payload["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if m, ok := msg.(map[string]interface{}); ok {
				if content, ok := m["content"].(string); ok {
					inputText += content
				}
			}
		}
	}

	// Very rough estimation - 4 characters per token
	return len(inputText) / 4, nil
}

// ProcessRequest sends a request to the Anthropic API
func (p *AnthropicProvider) ProcessRequest(ctx context.Context, req *Request) (*Response, error) {
	// Check if model is supported
	if !p.models[req.ModelName] {
		return nil, ErrModelNotFound
	}

	// Add model to payload
	payload := make(map[string]interface{})
	for k, v := range req.Payload {
		payload[k] = v
	}
	payload["model"] = req.ModelName

	// Prepare request body
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Prepare request headers
	headers := map[string]string{
		"Content-Type":      "application/json",
		"X-API-Key":         p.config.APIKey,
		"anthropic-version": "2023-06-01",
	}

	// Send request (always use messages endpoint for Claude)
	respBody, err := p.doRequest(ctx, "POST", p.config.BaseURL+"/messages", reqBody, headers)
	if err != nil {
		return nil, err
	}

	// Parse response
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	// Extract token usage
	tokensUsed := 0
	if usage, ok := result["usage"].(map[string]interface{}); ok {
		if inputTokens, ok := usage["input_tokens"].(float64); ok {
			tokensUsed += int(inputTokens)
		}
		if outputTokens, ok := usage["output_tokens"].(float64); ok {
			tokensUsed += int(outputTokens)
		}
	}

	return &Response{
		ProviderName: p.name,
		ModelName:    req.ModelName,
		Result:       result,
		TokensUsed:   tokensUsed,
	}, nil
}

// ProcessRequestAsync sends a request to the Anthropic API asynchronously
func (p *AnthropicProvider) ProcessRequestAsync(ctx context.Context, req *Request, responseCh chan<- *Response) {
	go func() {
		resp, err := p.ProcessRequest(ctx, req)
		if err != nil {
			responseCh <- &Response{
				ProviderName: p.name,
				ModelName:    req.ModelName,
				Error:        err,
			}
			return
		}
		responseCh <- resp
	}()
}

// GenericProvider implements the Provider interface for generic API providers
type GenericProvider struct {
	BaseProvider
}

// newGenericProvider creates a new generic provider
func newGenericProvider(name string, cfg config.ProviderSettings, httpClient *http.Client) (*GenericProvider, error) {
	base := newBaseProvider(name, cfg, httpClient)
	return &GenericProvider{
		BaseProvider: base,
	}, nil
}

// EstimateTokens provides a simple token estimation for generic providers
func (p *GenericProvider) EstimateTokens(req *Request) (int, error) {
	// Generic estimation - just count characters and divide by 4
	jsonBytes, err := json.Marshal(req.Payload)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal payload: %w", err)
	}
	return len(jsonBytes) / 4, nil
}

// ProcessRequest sends a request to a generic API
func (p *GenericProvider) ProcessRequest(ctx context.Context, req *Request) (*Response, error) {
	// Check if model is supported
	if !p.models[req.ModelName] {
		return nil, ErrModelNotFound
	}

	// Add model to payload
	payload := make(map[string]interface{})
	for k, v := range req.Payload {
		payload[k] = v
	}
	payload["model"] = req.ModelName

	// Prepare request body
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Prepare request headers
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + p.config.APIKey,
	}

	// Send request
	respBody, err := p.doRequest(ctx, "POST", p.config.BaseURL, reqBody, headers)
	if err != nil {
		return nil, err
	}

	// Parse response
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	// For generic providers, we don't know how to extract token usage
	// so we'll use our estimate
	tokensUsed, _ := p.EstimateTokens(req)

	return &Response{
		ProviderName: p.name,
		ModelName:    req.ModelName,
		Result:       result,
		TokensUsed:   tokensUsed,
	}, nil
}

// ProcessRequestAsync sends a request to a generic API asynchronously
func (p *GenericProvider) ProcessRequestAsync(ctx context.Context, req *Request, responseCh chan<- *Response) {
	go func() {
		resp, err := p.ProcessRequest(ctx, req)
		if err != nil {
			responseCh <- &Response{
				ProviderName: p.name,
				ModelName:    req.ModelName,
				Error:        err,
			}
			return
		}
		responseCh <- resp
	}()
}
