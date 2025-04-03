// Package limiter provides rate limiting functionality for the LLM Load Balancer.
//
// This file defines interfaces and common types used by different rate limiter
// implementations. Two primary implementations are supported:
// 1. In-memory token bucket (for single-instance deployments)
// 2. Redis-based distributed token bucket (for multi-instance deployments)
package limiter

import (
	"context"
	"errors"
	"time"
)

// Common errors returned by limiters
var (
	// ErrRateLimited is returned when a request exceeds the rate limit
	ErrRateLimited = errors.New("rate limited: insufficient tokens available")

	// ErrInvalidTokenCost is returned when the token cost is invalid
	ErrInvalidTokenCost = errors.New("invalid token cost: must be positive")

	// ErrBackendUnavailable is returned when the backend storage is unavailable
	ErrBackendUnavailable = errors.New("backend storage unavailable")
)

// LimiterType defines the type of limiter implementation
type LimiterType string

const (
	// InMemoryLimiter is a local in-memory token bucket implementation
	InMemoryLimiter LimiterType = "memory"

	// RedisLimiter is a distributed token bucket implementation backed by Redis
	RedisLimiter LimiterType = "redis"
)

// LimiterConfig holds the configuration for rate limiters
type LimiterConfig struct {
	// Type specifies the limiter implementation to use (memory or redis)
	Type LimiterType

	// TokensPerSecond defines the rate at which tokens are added to the bucket
	TokensPerSecond float64

	// BucketSize defines the maximum number of tokens that can be stored
	BucketSize int

	// RedisOptions contains Redis-specific configuration when using RedisLimiter
	RedisOptions *RedisOptions
}

// RedisOptions contains Redis-specific configuration for RedisLimiter
type RedisOptions struct {
	// KeyPrefix is used as a prefix for Redis keys to avoid collisions
	KeyPrefix string

	// KeyExpiry is the TTL for Redis keys
	KeyExpiry time.Duration

	// LuaScriptPath is the path to Lua script file used for atomic operations
	// If empty, embedded scripts will be used
	LuaScriptPath string
}

// Limiter defines the interface that all rate limiter implementations must implement
type Limiter interface {
	// Allow checks if a request with the given token cost is allowed.
	// It returns true if the request is allowed, false otherwise.
	// If the request is allowed, the tokens are consumed from the bucket.
	// An error is returned if the operation fails.
	Allow(ctx context.Context, tokenCost int) (bool, error)

	// AllowN is similar to Allow but takes a key to support different rate limits
	// for different resources (e.g., per user, per model, etc.)
	AllowN(ctx context.Context, key string, tokenCost int) (bool, error)

	// Wait blocks until the requested number of tokens are available or the context
	// is canceled. It returns nil if tokens were successfully consumed, otherwise
	// it returns an error explaining why the wait failed.
	Wait(ctx context.Context, tokenCost int) error

	// WaitN is similar to Wait but takes a key to support different rate limits
	// for different resources.
	WaitN(ctx context.Context, key string, tokenCost int) error

	// Reserve reserves the requested number of tokens and returns a Reservation
	// that can be used to wait until the tokens are available.
	Reserve(ctx context.Context, tokenCost int) (Reservation, error)

	// ReserveN is similar to Reserve but takes a key to support different rate limits
	// for different resources.
	ReserveN(ctx context.Context, key string, tokenCost int) (Reservation, error)

	// GetTokensAvailable returns the current number of tokens available.
	// For distributed implementations, this is a best-effort value and may not
	// be perfectly accurate due to concurrent operations.
	GetTokensAvailable(ctx context.Context) (int, error)

	// GetTokensAvailableN is similar to GetTokensAvailable but for a specific key.
	GetTokensAvailableN(ctx context.Context, key string) (int, error)
}

// Reservation represents a reservation of tokens from a Limiter
type Reservation interface {
	// OK returns true if the reservation was successful
	OK() bool

	// Tokens returns the number of tokens reserved
	Tokens() int

	// Wait blocks until the reserved tokens are available or the context is canceled
	Wait(ctx context.Context) error

	// DelayFrom returns the expected delay from the given time until the
	// reservation can be fulfilled
	DelayFrom(now time.Time) time.Duration

	// CancelAt cancels the reservation at the given time if it hasn't been used yet
	CancelAt(now time.Time)
}

// New creates a new rate limiter based on the provided configuration
func New(config LimiterConfig) (Limiter, error) {
	// This function will be implemented in token_bucket.go and redis_limiter.go
	// It will return the appropriate implementation based on config.Type
	return nil, errors.New("not implemented")
}
