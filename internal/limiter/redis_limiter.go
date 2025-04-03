// Package limiter provides rate limiting functionality for the LLM Load Balancer.
//
// This file implements a Redis-based distributed token bucket rate limiter
// for multi-instance deployments.
package limiter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// RedisTokenBucket implements the Limiter interface using Redis as a backend
// for distributed rate limiting across multiple instances
type RedisTokenBucket struct {
	// Redis client
	client redis.UniversalClient

	// Configuration
	tokensPerSecond float64
	bucketSize      int
	keyPrefix       string
	keyExpiry       time.Duration

	// Lua scripts for atomic operations
	allowScript        *redis.Script
	getAvailableScript *redis.Script
	reserveScript      *redis.Script

	// Wait queue (local to this instance)
	waiters     map[string]chan struct{}
	waitersByID map[string]string
	waitersMu   sync.Mutex
}

// redisReservation implements the Reservation interface for Redis limiter
type redisReservation struct {
	limiter       *RedisTokenBucket
	tokens        int
	timeToProcess time.Time
	key           string
	reservationID string
	ok            bool
}

// NewRedisTokenBucket creates a new Redis-based token bucket rate limiter
func NewRedisTokenBucket(config LimiterConfig, client redis.UniversalClient) (*RedisTokenBucket, error) {
	if client == nil {
		return nil, errors.New("redis client is required")
	}

	if config.TokensPerSecond <= 0 {
		config.TokensPerSecond = 1.0 // Default to 1 token per second
	}

	if config.BucketSize <= 0 {
		config.BucketSize = int(config.TokensPerSecond * 60) // Default to 1 minute worth of tokens
	}

	if config.RedisOptions == nil {
		config.RedisOptions = &RedisOptions{
			KeyPrefix: "limiter:",
			KeyExpiry: 24 * time.Hour,
		}
	}

	if config.RedisOptions.KeyPrefix == "" {
		config.RedisOptions.KeyPrefix = "limiter:"
	}

	if config.RedisOptions.KeyExpiry <= 0 {
		config.RedisOptions.KeyExpiry = 24 * time.Hour
	}

	// Initialize the limiter
	limiter := &RedisTokenBucket{
		client:          client,
		tokensPerSecond: config.TokensPerSecond,
		bucketSize:      config.BucketSize,
		keyPrefix:       config.RedisOptions.KeyPrefix,
		keyExpiry:       config.RedisOptions.KeyExpiry,
		waiters:         make(map[string]chan struct{}),
		waitersByID:     make(map[string]string),
	}

	// Load the Lua scripts
	limiter.loadScripts()

	utils.Info("Redis token bucket initialized", map[string]interface{}{
		"tokens_per_second": config.TokensPerSecond,
		"bucket_size":       config.BucketSize,
		"key_prefix":        config.RedisOptions.KeyPrefix,
		"key_expiry":        config.RedisOptions.KeyExpiry.String(),
	})

	return limiter, nil
}

// loadScripts loads the Lua scripts used for atomic operations
func (rl *RedisTokenBucket) loadScripts() {
	// Script for the Allow operation
	// KEYS[1] = bucket key
	// KEYS[2] = last refill timestamp key
	// ARGV[1] = bucket size
	// ARGV[2] = tokens per second
	// ARGV[3] = current timestamp (seconds since epoch)
	// ARGV[4] = token cost
	// ARGV[5] = key expiry in seconds
	allowScript := `
		local bucket_key = KEYS[1]
		local timestamp_key = KEYS[2]
		local bucket_size = tonumber(ARGV[1])
		local tokens_per_sec = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])
		local token_cost = tonumber(ARGV[4])
		local key_expiry = tonumber(ARGV[5])

		-- Get the last refill timestamp and current tokens
		local last_refill = tonumber(redis.call('GET', timestamp_key) or now)
		local current_tokens = tonumber(redis.call('GET', bucket_key) or bucket_size)

		-- Calculate the number of tokens to add based on elapsed time
		local elapsed_seconds = now - last_refill
		local tokens_to_add = elapsed_seconds * tokens_per_sec
		
		-- Refill the bucket
		current_tokens = math.min(bucket_size, current_tokens + tokens_to_add)
		
		-- Update the last refill timestamp
		redis.call('SET', timestamp_key, now, 'EX', key_expiry)
		
		-- Check if we have enough tokens
		if current_tokens >= token_cost then
			-- Consume tokens
			current_tokens = current_tokens - token_cost
			redis.call('SET', bucket_key, current_tokens, 'EX', key_expiry)
			return 1 -- Success
		else
			-- Store the current tokens without consuming
			redis.call('SET', bucket_key, current_tokens, 'EX', key_expiry)
			return 0 -- Not enough tokens
		end
	`
	rl.allowScript = redis.NewScript(allowScript)

	// Script to get available tokens
	// KEYS[1] = bucket key
	// KEYS[2] = last refill timestamp key
	// ARGV[1] = bucket size
	// ARGV[2] = tokens per second
	// ARGV[3] = current timestamp (seconds since epoch)
	// ARGV[4] = key expiry in seconds
	getAvailableScript := `
		local bucket_key = KEYS[1]
		local timestamp_key = KEYS[2]
		local bucket_size = tonumber(ARGV[1])
		local tokens_per_sec = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])
		local key_expiry = tonumber(ARGV[4])

		-- Get the last refill timestamp and current tokens
		local last_refill = tonumber(redis.call('GET', timestamp_key) or now)
		local current_tokens = tonumber(redis.call('GET', bucket_key) or bucket_size)

		-- Calculate the number of tokens to add based on elapsed time
		local elapsed_seconds = now - last_refill
		local tokens_to_add = elapsed_seconds * tokens_per_sec
		
		-- Calculate the new token count
		local new_tokens = math.min(bucket_size, current_tokens + tokens_to_add)
		
		-- Update the last refill timestamp and token count
		redis.call('SET', timestamp_key, now, 'EX', key_expiry)
		redis.call('SET', bucket_key, new_tokens, 'EX', key_expiry)
		
		return new_tokens
	`
	rl.getAvailableScript = redis.NewScript(getAvailableScript)

	// Script for the Reserve operation
	// KEYS[1] = bucket key
	// KEYS[2] = last refill timestamp key
	// ARGV[1] = bucket size
	// ARGV[2] = tokens per second
	// ARGV[3] = current timestamp (seconds since epoch)
	// ARGV[4] = token cost
	// ARGV[5] = key expiry in seconds
	reserveScript := `
		local bucket_key = KEYS[1]
		local timestamp_key = KEYS[2]
		local bucket_size = tonumber(ARGV[1])
		local tokens_per_sec = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])
		local token_cost = tonumber(ARGV[4])
		local key_expiry = tonumber(ARGV[5])

		-- Get the last refill timestamp and current tokens
		local last_refill = tonumber(redis.call('GET', timestamp_key) or now)
		local current_tokens = tonumber(redis.call('GET', bucket_key) or bucket_size)

		-- Calculate the number of tokens to add based on elapsed time
		local elapsed_seconds = now - last_refill
		local tokens_to_add = elapsed_seconds * tokens_per_sec
		
		-- Refill the bucket
		current_tokens = math.min(bucket_size, current_tokens + tokens_to_add)
		
		-- Update the last refill timestamp
		redis.call('SET', timestamp_key, now, 'EX', key_expiry)
		
		-- Calculate how long to wait if we don't have enough tokens
		if current_tokens >= token_cost then
			-- We have enough tokens now
			current_tokens = current_tokens - token_cost
			redis.call('SET', bucket_key, current_tokens, 'EX', key_expiry)
			return "0" -- No wait needed
		else
			-- Store the current tokens without consuming
			redis.call('SET', bucket_key, current_tokens, 'EX', key_expiry)
			-- Calculate wait time in milliseconds
			local tokens_needed = token_cost - current_tokens
			local wait_seconds = tokens_needed / tokens_per_sec
			local wait_ms = math.ceil(wait_seconds * 1000)
			return tostring(wait_ms)
		end
	`
	rl.reserveScript = redis.NewScript(reserveScript)
}

// getBucketKey gets the Redis key for a token bucket
func (rl *RedisTokenBucket) getBucketKey(key string) string {
	return rl.keyPrefix + "bucket:" + key
}

// getTimestampKey gets the Redis key for the last refill timestamp
func (rl *RedisTokenBucket) getTimestampKey(key string) string {
	return rl.keyPrefix + "timestamp:" + key
}

// Allow checks if a request with the given token cost is allowed
func (rl *RedisTokenBucket) Allow(ctx context.Context, tokenCost int) (bool, error) {
	return rl.AllowN(ctx, "default", tokenCost)
}

// AllowN checks if a request with the given key and token cost is allowed
func (rl *RedisTokenBucket) AllowN(ctx context.Context, key string, tokenCost int) (bool, error) {
	if tokenCost <= 0 {
		return false, ErrInvalidTokenCost
	}

	bucketKey := rl.getBucketKey(key)
	timestampKey := rl.getTimestampKey(key)
	now := float64(time.Now().Unix())
	keyExpirySeconds := int(rl.keyExpiry.Seconds())

	// Execute the allow script
	result, err := rl.allowScript.Run(
		ctx,
		rl.client,
		[]string{bucketKey, timestampKey},
		rl.bucketSize,
		rl.tokensPerSecond,
		now,
		tokenCost,
		keyExpirySeconds,
	).Int()

	if err != nil {
		utils.ErrorContext(ctx, err, "Redis token bucket operation failed", map[string]interface{}{
			"key":       key,
			"operation": "Allow",
		})
		return false, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	return result == 1, nil // 1 means success
}

// Wait blocks until tokens are available or context is canceled
func (rl *RedisTokenBucket) Wait(ctx context.Context, tokenCost int) error {
	return rl.WaitN(ctx, "default", tokenCost)
}

// WaitN blocks until tokens are available for the given key or context is canceled
func (rl *RedisTokenBucket) WaitN(ctx context.Context, key string, tokenCost int) error {
	if tokenCost <= 0 {
		return ErrInvalidTokenCost
	}

	// Try to consume tokens immediately
	if allowed, err := rl.AllowN(ctx, key, tokenCost); err != nil {
		return err
	} else if allowed {
		return nil
	}

	// If that fails, make a reservation and wait
	reservation, err := rl.ReserveN(ctx, key, tokenCost)
	if err != nil {
		return err
	}

	return reservation.Wait(ctx)
}

// Reserve reserves tokens
func (rl *RedisTokenBucket) Reserve(ctx context.Context, tokenCost int) (Reservation, error) {
	return rl.ReserveN(ctx, "default", tokenCost)
}

// ReserveN reserves tokens for a specific key
func (rl *RedisTokenBucket) ReserveN(ctx context.Context, key string, tokenCost int) (Reservation, error) {
	if tokenCost <= 0 {
		return nil, ErrInvalidTokenCost
	}

	bucketKey := rl.getBucketKey(key)
	timestampKey := rl.getTimestampKey(key)
	now := float64(time.Now().Unix())
	keyExpirySeconds := int(rl.keyExpiry.Seconds())

	// Execute the reserve script
	waitMsStr, err := rl.reserveScript.Run(
		ctx,
		rl.client,
		[]string{bucketKey, timestampKey},
		rl.bucketSize,
		rl.tokensPerSecond,
		now,
		tokenCost,
		keyExpirySeconds,
	).Text()

	if err != nil {
		utils.ErrorContext(ctx, err, "Redis token bucket reservation failed", map[string]interface{}{
			"key":       key,
			"operation": "Reserve",
		})
		return nil, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	waitMs, err := strconv.ParseInt(waitMsStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid response from Redis: %v", err)
	}

	// If waitMs is 0, tokens were consumed immediately
	if waitMs == 0 {
		return &redisReservation{
			limiter:       rl,
			tokens:        tokenCost,
			timeToProcess: time.Now(),
			key:           key,
			reservationID: utils.GenerateShortID(),
			ok:            true,
		}, nil
	}

	// Create a reservation with a future processing time
	reservationID := utils.GenerateShortID()
	timeToProcess := time.Now().Add(time.Duration(waitMs) * time.Millisecond)

	// Create a channel for this reservation
	waiterKey := key + ":" + reservationID
	rl.waitersMu.Lock()
	waitCh := make(chan struct{})
	rl.waiters[waiterKey] = waitCh
	rl.waitersByID[reservationID] = waiterKey
	rl.waitersMu.Unlock()

	// Schedule the token check and notification
	go func() {
		// Create a timeout context for background operations
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(waitMs)*time.Millisecond+5*time.Second)
		defer cancel()

		time.Sleep(time.Duration(waitMs) * time.Millisecond)

		// Use the timeout context for the AllowN call
		allowed, err := rl.AllowN(ctx, key, tokenCost)
		if err != nil {
			utils.ErrorContext(ctx, err, "Redis token bucket reservation failed", map[string]interface{}{
				"key":       key,
				"operation": "AllowN",
			})
			return
		}

		if allowed {
			rl.waitersMu.Lock()
			if ch, ok := rl.waiters[waiterKey]; ok {
				close(ch)
				delete(rl.waiters, waiterKey)
				delete(rl.waitersByID, reservationID)
			}
			rl.waitersMu.Unlock()
		} else {
			rl.waitersMu.Lock()
			if ch, ok := rl.waiters[waiterKey]; ok {
				close(ch)
				delete(rl.waiters, waiterKey)
				delete(rl.waitersByID, reservationID)
			}
			rl.waitersMu.Unlock()

			utils.WarnContext(ctx, "Failed to acquire tokens",
				map[string]interface{}{
					"key":        key,
					"token_cost": tokenCost,
				})
		}
	}()

	return &redisReservation{
		limiter:       rl,
		tokens:        tokenCost,
		timeToProcess: timeToProcess,
		key:           key,
		reservationID: reservationID,
		ok:            true,
	}, nil
}

// GetTokensAvailable returns the current number of tokens available
func (rl *RedisTokenBucket) GetTokensAvailable(ctx context.Context) (int, error) {
	return rl.GetTokensAvailableN(ctx, "default")
}

// GetTokensAvailableN returns the current number of tokens available for a specific key
func (rl *RedisTokenBucket) GetTokensAvailableN(ctx context.Context, key string) (int, error) {
	bucketKey := rl.getBucketKey(key)
	timestampKey := rl.getTimestampKey(key)
	now := float64(time.Now().Unix())
	keyExpirySeconds := int(rl.keyExpiry.Seconds())

	// Execute the script to get available tokens
	tokens, err := rl.getAvailableScript.Run(
		ctx,
		rl.client,
		[]string{bucketKey, timestampKey},
		rl.bucketSize,
		rl.tokensPerSecond,
		now,
		keyExpirySeconds,
	).Int()

	if err != nil {
		if err == redis.Nil {
			// Key doesn't exist, return the full bucket size
			return rl.bucketSize, nil
		}
		utils.ErrorContext(ctx, err, "Redis get tokens available failed", map[string]interface{}{
			"key":       key,
			"operation": "GetTokensAvailable",
		})
		return 0, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	return tokens, nil
}

// redisReservation implementation

// OK returns true if the reservation was successful
func (r *redisReservation) OK() bool {
	return r.ok
}

// Tokens returns the number of tokens reserved
func (r *redisReservation) Tokens() int {
	return r.tokens
}

// Wait blocks until the reservation is fulfilled or the context is canceled
func (r *redisReservation) Wait(ctx context.Context) error {
	if !r.ok {
		return ErrRateLimited
	}

	// If the reservation is already fulfilled
	if time.Now().After(r.timeToProcess) {
		return nil
	}

	// Create a timeout slightly longer than expected wait time
	waitDeadline := r.timeToProcess.Add(5 * time.Second)
	timeoutCtx, cancel := context.WithDeadline(ctx, waitDeadline)
	defer cancel()

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

	// Add timeout case to select statement
	select {
	case <-ctx.Done():
		r.CancelAt(time.Now())
		return ctx.Err()
	case <-timeoutCtx.Done():
		if time.Now().After(r.timeToProcess) {
			// We've waited long enough, assume tokens are available
			return nil
		}
		r.CancelAt(time.Now())
		return fmt.Errorf("%w: timed out waiting for tokens", ErrRateLimited)
	case <-waitCh:
		return nil
	}
}

// DelayFrom returns the expected delay from the given time
func (r *redisReservation) DelayFrom(now time.Time) time.Duration {
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
func (r *redisReservation) CancelAt(now time.Time) {
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

// NewRedisLimiterWithClient creates a new Redis-based token bucket rate limiter
func NewRedisLimiterWithClient(config LimiterConfig, client redis.UniversalClient) (Limiter, error) {
	if config.Type != RedisLimiter {
		return nil, errors.New("config type must be RedisLimiter")
	}

	return NewRedisTokenBucket(config, client)
}
