// Package proxy provides reverse proxy functionality for forwarding requests to LLM provider APIs.
//
// This file handles:
// - Reverse proxy logic to forward client requests to the appropriate LLM provider API
// - Header manipulation for authentication and request tracking
// - Response modification (if needed)
// - Metrics collection for request/response timing
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// RequestModifier is a function that can modify a request before it's sent
type RequestModifier func(*http.Request) error

// ResponseModifier is a function that can modify a response after it's received
type ResponseModifier func(*http.Response) error

// RequestMetrics contains timing and size information for a request
type RequestMetrics struct {
	StartTime      time.Time
	EndTime        time.Time
	Duration       time.Duration
	RequestSize    int64
	ResponseSize   int64
	StatusCode     int
	ProviderName   string
	ModelName      string
	RequestID      string
	EndpointPath   string
	ResponseTokens int
}

// ReverseProxy handles forwarding requests to the appropriate backend LLM API
type ReverseProxy struct {
	requestModifiers  []RequestModifier
	responseModifiers []ResponseModifier
	transport         http.RoundTripper
}

// NewReverseProxy creates a new reverse proxy with the given options
func NewReverseProxy(options ...Option) *ReverseProxy {
	proxy := &ReverseProxy{
		transport: http.DefaultTransport,
	}

	// Apply options
	for _, option := range options {
		option(proxy)
	}

	return proxy
}

// Option is a function that configures a ReverseProxy
type Option func(*ReverseProxy)

// WithTransport sets the proxy's transport
func WithTransport(transport http.RoundTripper) Option {
	return func(p *ReverseProxy) {
		p.transport = transport
	}
}

// WithRequestModifier adds a request modifier
func WithRequestModifier(modifier RequestModifier) Option {
	return func(p *ReverseProxy) {
		p.requestModifiers = append(p.requestModifiers, modifier)
	}
}

// WithResponseModifier adds a response modifier
func WithResponseModifier(modifier ResponseModifier) Option {
	return func(p *ReverseProxy) {
		p.responseModifiers = append(p.responseModifiers, modifier)
	}
}

// Forward forwards a request to the target URL and returns the response
func (p *ReverseProxy) Forward(ctx context.Context, req *http.Request, targetURL string) (*http.Response, error) {
	// Parse the target URL
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	// Create metrics for this request
	metrics := &RequestMetrics{
		StartTime:    time.Now(),
		EndpointPath: req.URL.Path,
		RequestID:    req.Header.Get("X-Request-ID"),
	}

	// Extract model info from request URL or headers if available
	if provider := req.Header.Get("X-Provider"); provider != "" {
		metrics.ProviderName = provider
	}
	if model := req.Header.Get("X-Model"); model != "" {
		metrics.ModelName = model
	}

	// Clone the request to avoid modifying the original
	outReq := req.Clone(ctx)
	outReq.URL = parsedURL.ResolveReference(req.URL)
	outReq.Host = parsedURL.Host

	// Track request size if body is present
	if req.Body != nil {
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}

		// Remember the size
		metrics.RequestSize = int64(len(bodyBytes))

		// Create a new body from the bytes
		outReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes)) // Restore original request body
	}

	// Apply request modifiers
	for _, modifier := range p.requestModifiers {
		if err := modifier(outReq); err != nil {
			return nil, fmt.Errorf("request modifier failed: %w", err)
		}
	}

	// Log the outgoing request
	utils.InfoContext(ctx, "Forwarding request", map[string]interface{}{
		"method":       outReq.Method,
		"target_url":   outReq.URL.String(),
		"request_id":   metrics.RequestID,
		"provider":     metrics.ProviderName,
		"model":        metrics.ModelName,
		"request_size": metrics.RequestSize,
	})

	// Make the request
	resp, err := p.transport.RoundTrip(outReq)
	metrics.EndTime = time.Now()
	metrics.Duration = metrics.EndTime.Sub(metrics.StartTime)

	if err != nil {
		utils.ErrorContext(ctx, err, "Request failed", map[string]interface{}{
			"duration_ms": metrics.Duration.Milliseconds(),
			"target_url":  outReq.URL.String(),
			"request_id":  metrics.RequestID,
		})
		return nil, fmt.Errorf("request failed: %w", err)
	}

	// Record metrics from response
	metrics.StatusCode = resp.StatusCode

	// Apply response modifiers
	for _, modifier := range p.responseModifiers {
		if err := modifier(resp); err != nil {
			utils.WarnContext(ctx, "Response modifier failed", map[string]interface{}{
				"error":      err.Error(),
				"request_id": metrics.RequestID,
			})
			// Continue with other modifiers
		}
	}

	// Track response size by creating a buffer copy if needed
	if resp.Body != nil {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}

		// Remember the size
		metrics.ResponseSize = int64(len(bodyBytes))

		// Create a new body from the bytes
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Try to extract token usage information if present in JSON response
		if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			// This is a simplified version - a real implementation would properly parse the JSON
			if tokenIndex := bytes.Index(bodyBytes, []byte(`"total_tokens":`)); tokenIndex > 0 {
				// Very simplistic parsing - real code would use json.Unmarshal
				tokenStr := bodyBytes[tokenIndex+15 : tokenIndex+30]
				if endIndex := bytes.IndexAny(tokenStr, ",}"); endIndex > 0 {
					tokenStr = tokenStr[:endIndex]
					if tokenCount, err := strconv.Atoi(string(bytes.TrimSpace(tokenStr))); err == nil {
						metrics.ResponseTokens = tokenCount
					}
				}
			}
		}
	}

	// Log the response
	utils.InfoContext(ctx, "Received response", map[string]interface{}{
		"status_code":   metrics.StatusCode,
		"duration_ms":   metrics.Duration.Milliseconds(),
		"request_id":    metrics.RequestID,
		"response_size": metrics.ResponseSize,
		"tokens":        metrics.ResponseTokens,
	})

	return resp, nil
}

// ProxyHandler creates an http.Handler that forwards requests to a target URL
func (p *ReverseProxy) ProxyHandler(targetURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := p.Forward(r.Context(), r, targetURL)
		if err != nil {
			http.Error(w, fmt.Sprintf("Proxy error: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy headers
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		// Set status code
		w.WriteHeader(resp.StatusCode)

		// Copy body
		if _, err := io.Copy(w, resp.Body); err != nil {
			utils.ErrorContext(r.Context(), err, "Failed to copy response body", nil)
			// Can't write an error response at this point as headers already sent
		}
	})
}

// CreateDynamicProxy creates a reverse proxy that determines the target URL based on the request
func CreateDynamicProxy(targetResolver func(*http.Request) (string, error)) http.Handler {
	// Create a basic reverse proxy
	proxy := NewReverseProxy()

	// Return a handler that uses the resolver
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Determine target URL
		targetURL, err := targetResolver(r)
		if err != nil {
			http.Error(w, fmt.Sprintf("Routing error: %v", err), http.StatusBadRequest)
			return
		}

		// Forward the request
		resp, err := proxy.Forward(r.Context(), r, targetURL)
		if err != nil {
			http.Error(w, fmt.Sprintf("Proxy error: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy headers
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		// Set status code
		w.WriteHeader(resp.StatusCode)

		// Copy body
		if _, err := io.Copy(w, resp.Body); err != nil {
			utils.ErrorContext(r.Context(), err, "Failed to copy response body", nil)
			// Can't write an error response at this point as headers already sent
		}
	})
}

// Common request modifiers

// AddAuthHeader adds an authorization header to the request
func AddAuthHeader(authType, authValue string) RequestModifier {
	return func(req *http.Request) error {
		req.Header.Set("Authorization", fmt.Sprintf("%s %s", authType, authValue))
		return nil
	}
}

// AddRequestHeaders adds custom headers to the request
func AddRequestHeaders(headers map[string]string) RequestModifier {
	return func(req *http.Request) error {
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		return nil
	}
}

// Common response modifiers

// ModifyResponseHeader modifies a response header
func ModifyResponseHeader(key, value string) ResponseModifier {
	return func(resp *http.Response) error {
		resp.Header.Set(key, value)
		return nil
	}
}

// CreateStandardReverseProxy creates a reverse proxy using httputil.ReverseProxy
// This is a more standard implementation that doesn't allow for metrics collection
func CreateStandardReverseProxy(targetURL string) (*httputil.ReverseProxy, error) {
	url, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}

	return httputil.NewSingleHostReverseProxy(url), nil
}
