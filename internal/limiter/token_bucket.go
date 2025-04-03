// Package limiter provides rate limiting functionality for the LLM Load Balancer.
//
// This file implements an in-memory token bucket rate limiter for single-instance deployments.
package limiter

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// TokenBucket implements the Limiter interface using an in-memory token bucket algorithm
type TokenBucket struct {
	// Configuration
	tokensPerSecond float64 // Rate at which tokens are added
	bucketSize      int     // Maximum number of tokens the bucket can hold

	// State
	availableTokens float64    // Current number of tokens (float64 for partial tokens)
	lastRefillTime  time.Time  // Last time the bucket was refilled
	mu              sync.Mutex // Mutex for thread safety

	// Wait queue
	waiters     map[string]chan struct{} // Channels to notify waiters
	waitersByID map[string]string        // Map from reservation ID to waiter key
	waitersMu   sync.Mutex               // Separate mutex for waiters to avoid deadlocks
}

// tokenBucketReservation implements the Reservation interface
type tokenBucketReservation struct {
	limiter       *TokenBucket
	tokens        int
	timeToProcess time.Time
	reservationID string
	ok            bool
}

// NewTokenBucket creates a new token bucket rate limiter
func NewTokenBucket(config LimiterConfig) (*TokenBucket, error) {
	if config.TokensPerSecond <= 0 {
		config.TokensPerSecond = 1.0 // Default to 1 token per second
	}

	if config.BucketSize <= 0 {
		config.BucketSize = int(config.TokensPerSecond * 60) // Default to 1 minute worth of tokens
	}

	tb := &TokenBucket{
		tokensPerSecond: config.TokensPerSecond,
		bucketSize:      config.BucketSize,
		availableTokens: float64(config.BucketSize), // Start with a full bucket
		lastRefillTime:  time.Now(),
		waiters:         make(map[string]chan struct{}),
		waitersByID:     make(map[string]string),
	}

	utils.Info("In-memory token bucket initialized", map[string]interface{}{
		"tokens_per_second": config.TokensPerSecond,
		"bucket_size":       config.BucketSize,
	})

	return tb, nil
}

// refill adds tokens to the bucket based on elapsed time
// Must be called with the mutex held
func (tb *TokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(tb.lastRefillTime).Seconds()
	tb.lastRefillTime = now

	// Calculate tokens to add
	tokensToAdd := elapsed * tb.tokensPerSecond
	tb.availableTokens = math.Min(float64(tb.bucketSize), tb.availableTokens+tokensToAdd)
}

// Allow checks if a request with the given token cost is allowed
func (tb *TokenBucket) Allow(ctx context.Context, tokenCost int) (bool, error) {
	return tb.AllowN(ctx, "default", tokenCost)
}

// AllowN checks if a request with the given key and token cost is allowed
func (tb *TokenBucket) AllowN(ctx context.Context, key string, tokenCost int) (bool, error) {
	if tokenCost <= 0 {
		return false, ErrInvalidTokenCost
	}

	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Refill the bucket based on elapsed time
	tb.refill()

	// Check if we have enough tokens
	if tb.availableTokens >= float64(tokenCost) {
		tb.availableTokens -= float64(tokenCost)
		return true, nil
	}

	return false, nil
}

// Wait blocks until tokens are available or context is canceled
func (tb *TokenBucket) Wait(ctx context.Context, tokenCost int) error {
	return tb.WaitN(ctx, "default", tokenCost)
}

// WaitN blocks until tokens are available for the given key or context is canceled
func (tb *TokenBucket) WaitN(ctx context.Context, key string, tokenCost int) error {
	if tokenCost <= 0 {
		return ErrInvalidTokenCost
	}

	// Try to consume tokens immediately first
	if allowed, _ := tb.AllowN(ctx, key, tokenCost); allowed {
		return nil
	}

	// If that fails, make a reservation and wait
	reservation, err := tb.ReserveN(ctx, key, tokenCost)
	if err != nil {
		return err
	}

	return reservation.Wait(ctx)
}

// Reserve reserves tokens
func (tb *TokenBucket) Reserve(ctx context.Context, tokenCost int) (Reservation, error) {
	return tb.ReserveN(ctx, "default", tokenCost)
}

// ReserveN reserves tokens for a specific key
func (tb *TokenBucket) ReserveN(ctx context.Context, key string, tokenCost int) (Reservation, error) {
	if tokenCost <= 0 {
		return nil, ErrInvalidTokenCost
	}

	tb.mu.Lock()

	// Refill the bucket
	tb.refill()

	// Calculate when enough tokens will be available
	tokensNeeded := float64(tokenCost) - tb.availableTokens
	var waitTime time.Duration

	if tokensNeeded <= 0 {
		// We have enough tokens now
		tb.availableTokens -= float64(tokenCost)
		tb.mu.Unlock()

		return &tokenBucketReservation{
			limiter:       tb,
			tokens:        tokenCost,
			timeToProcess: time.Now(),
			reservationID: utils.GenerateShortID(),
			ok:            true,
		}, nil
	}

	// Calculate wait time
	waitTime = time.Duration(tokensNeeded / tb.tokensPerSecond * float64(time.Second))
	timeToProcess := time.Now().Add(waitTime)

	// Create reservation
	reservationID := utils.GenerateShortID()

	// Create a combined key for the waiter
	waiterKey := key + ":" + reservationID

	// Create a channel for this reservation
	tb.waitersMu.Lock()
	waitCh := make(chan struct{})
	tb.waiters[waiterKey] = waitCh
	tb.waitersByID[reservationID] = waiterKey
	tb.waitersMu.Unlock()

	tb.mu.Unlock()

	// Schedule the token addition and notification
	go func() {
		time.Sleep(waitTime)

		tb.mu.Lock()
		// Only consume tokens if they're available (someone might have canceled)
		if tb.availableTokens >= float64(tokenCost) {
			tb.availableTokens -= float64(tokenCost)
			tb.mu.Unlock()

			// Notify the waiter
			tb.waitersMu.Lock()
			if ch, ok := tb.waiters[waiterKey]; ok {
				close(ch)
				delete(tb.waiters, waiterKey)
				delete(tb.waitersByID, reservationID)
			}
			tb.waitersMu.Unlock()
		} else {
			tb.mu.Unlock()
		}
	}()

	return &tokenBucketReservation{
		limiter:       tb,
		tokens:        tokenCost,
		timeToProcess: timeToProcess,
		reservationID: reservationID,
		ok:            true,
	}, nil
}

// GetTokensAvailable returns the current number of tokens available
func (tb *TokenBucket) GetTokensAvailable(ctx context.Context) (int, error) {
	return tb.GetTokensAvailableN(ctx, "default")
}

// GetTokensAvailableN returns the current number of tokens available for a specific key
func (tb *TokenBucket) GetTokensAvailableN(ctx context.Context, key string) (int, error) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Refill the bucket
	tb.refill()

	return int(tb.availableTokens), nil
}

// tokenBucketReservation implementation

// OK returns true if the reservation was successful
func (r *tokenBucketReservation) OK() bool {
	return r.ok
}

// Tokens returns the number of tokens reserved
func (r *tokenBucketReservation) Tokens() int {
	return r.tokens
}

// Wait blocks until the reservation is fulfilled or the context is canceled
func (r *tokenBucketReservation) Wait(ctx context.Context) error {
	if !r.ok {
		return ErrRateLimited
	}

	// If the reservation is already fulfilled
	if time.Now().After(r.timeToProcess) {
		return nil
	}

	// Get the wait channel
	r.limiter.waitersMu.Lock()
	waiterKey, exists := r.limiter.waitersByID[r.reservationID]
	if !exists {
		r.limiter.waitersMu.Unlock()
		return ErrRateLimited
	}

	waitCh, exists := r.limiter.waiters[waiterKey]
	r.limiter.waitersMu.Unlock()

	if !exists {
		return ErrRateLimited
	}

	// Wait for either the context to be canceled or the channel to be closed
	select {
	case <-ctx.Done():
		// Context was canceled, clean up the reservation
		r.CancelAt(time.Now())
		return ctx.Err()
	case <-waitCh:
		// Reservation fulfilled
		return nil
	}
}

// DelayFrom returns the expected delay from the given time
func (r *tokenBucketReservation) DelayFrom(now time.Time) time.Duration {
	if !r.ok {
		return 0
	}

	delay := r.timeToProcess.Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

// CancelAt cancels the reservation
func (r *tokenBucketReservation) CancelAt(now time.Time) {
	if !r.ok {
		return
	}

	// Only cancel if the reservation hasn't been fulfilled yet
	if now.Before(r.timeToProcess) {
		r.limiter.waitersMu.Lock()
		defer r.limiter.waitersMu.Unlock()

		waiterKey, exists := r.limiter.waitersByID[r.reservationID]
		if !exists {
			return
		}

		// Remove the waiter
		delete(r.limiter.waiters, waiterKey)
		delete(r.limiter.waitersByID, r.reservationID)

		// Mark as canceled
		r.ok = false
	}
}
