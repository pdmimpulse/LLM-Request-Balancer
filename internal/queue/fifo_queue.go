// Package queue provides queue functionality for the LLM Load Balancer.
//
// This file implements a FIFO (First-In-First-Out) queue for storing
// requests that are waiting for rate limiting token availability.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// Common errors returned by queue operations
var (
	// ErrQueueFull is returned when the queue has reached its maximum capacity
	ErrQueueFull = errors.New("queue is full")

	// ErrQueueEmpty is returned when trying to dequeue from an empty queue
	ErrQueueEmpty = errors.New("queue is empty")

	// ErrBackendUnavailable is returned when the backend storage is unavailable
	ErrBackendUnavailable = errors.New("backend storage unavailable")
)

// QueueType defines the type of queue implementation
type QueueType string

const (
	// InMemoryQueue is a local in-memory queue implementation
	InMemoryQueue QueueType = "memory"

	// RedisQueue is a distributed queue implementation backed by Redis
	RedisQueue QueueType = "redis"
)

// QueueConfig holds the configuration for the FIFO queue
type QueueConfig struct {
	// Type specifies the queue implementation to use (memory or redis)
	Type QueueType

	// MaxSize defines the maximum number of items that can be in the queue
	MaxSize int

	// ItemTTL defines how long items remain in the queue before expiring
	ItemTTL time.Duration

	// RedisOptions contains Redis-specific configuration when using RedisQueue
	RedisOptions *RedisOptions
}

// RedisOptions contains Redis-specific configuration for RedisQueue
type RedisOptions struct {
	// KeyPrefix is used as a prefix for Redis keys to avoid collisions
	KeyPrefix string

	// KeyExpiry is the TTL for Redis keys
	KeyExpiry time.Duration
}

// QueueItem represents an item in the queue
type QueueItem struct {
	// ID is a unique identifier for the queue item
	ID string `json:"id"`

	// Data is the payload of the queue item (typically a serialized request)
	Data []byte `json:"data"`

	// Metadata is additional information about the queue item
	Metadata map[string]interface{} `json:"metadata,omitempty"`

	// Priority defines the priority of the item (lower number = higher priority)
	Priority int `json:"priority"`

	// EnqueueTime is when the item was added to the queue
	EnqueueTime time.Time `json:"enqueue_time"`

	// ProcessAfter defines when the item should be processed
	// Used for implementing delay/schedule functionality
	ProcessAfter time.Time `json:"process_after,omitempty"`
}

// Queue defines the interface that all queue implementations must implement
type Queue interface {
	// Enqueue adds an item to the queue
	// Returns ErrQueueFull if the queue is at capacity
	Enqueue(ctx context.Context, item *QueueItem) error

	// Dequeue removes and returns the next item from the queue
	// Returns ErrQueueEmpty if the queue is empty
	Dequeue(ctx context.Context) (*QueueItem, error)

	// Peek returns the next item without removing it from the queue
	// Returns ErrQueueEmpty if the queue is empty
	Peek(ctx context.Context) (*QueueItem, error)

	// Size returns the current number of items in the queue
	Size(ctx context.Context) (int, error)

	// Clear removes all items from the queue
	Clear(ctx context.Context) error

	// Close releases any resources used by the queue
	Close() error
}

// InMemoryFIFO implements the Queue interface using an in-memory slice
type InMemoryFIFO struct {
	items    []*QueueItem
	maxSize  int
	itemTTL  time.Duration
	mu       sync.Mutex
	notEmpty *sync.Cond
}

// NewInMemoryQueue creates a new in-memory FIFO queue
func NewInMemoryQueue(config QueueConfig) (*InMemoryFIFO, error) {
	if config.MaxSize <= 0 {
		config.MaxSize = 1000 // Default to 1000 items
	}

	if config.ItemTTL <= 0 {
		config.ItemTTL = 24 * time.Hour // Default to 24 hours
	}

	q := &InMemoryFIFO{
		items:   make([]*QueueItem, 0, config.MaxSize),
		maxSize: config.MaxSize,
		itemTTL: config.ItemTTL,
	}
	q.notEmpty = sync.NewCond(&q.mu)

	// Start a background goroutine to clean up expired items
	go q.cleanupExpired()

	utils.Info("In-memory FIFO queue initialized", map[string]interface{}{
		"max_size": config.MaxSize,
		"item_ttl": config.ItemTTL.String(),
	})

	return q, nil
}

// cleanupExpired removes expired items from the queue
func (q *InMemoryFIFO) cleanupExpired() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		q.mu.Lock()
		newItems := make([]*QueueItem, 0, len(q.items))
		expiredCount := 0

		for _, item := range q.items {
			if now.Sub(item.EnqueueTime) < q.itemTTL {
				newItems = append(newItems, item)
			} else {
				expiredCount++
			}
		}

		if expiredCount > 0 {
			q.items = newItems
			utils.Info("Removed expired items from queue", map[string]interface{}{
				"expired_count": expiredCount,
				"remaining":     len(newItems),
			})
		}
		q.mu.Unlock()
	}
}

// Enqueue adds an item to the queue
func (q *InMemoryFIFO) Enqueue(ctx context.Context, item *QueueItem) error {
	if item == nil {
		return errors.New("cannot enqueue nil item")
	}

	// Ensure the EnqueueTime is set
	if item.EnqueueTime.IsZero() {
		item.EnqueueTime = time.Now()
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) >= q.maxSize {
		return ErrQueueFull
	}

	q.items = append(q.items, item)
	q.notEmpty.Signal() // Signal that the queue is not empty

	return nil
}

// Dequeue removes and returns the next item from the queue
func (q *InMemoryFIFO) Dequeue(ctx context.Context) (*QueueItem, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 {
		done := make(chan struct{})
		go func() {
			<-ctx.Done()
			q.notEmpty.Signal() // Wake up the waiting goroutine
			close(done)
		}()

		// Wait for an item to be added or context to be canceled
		q.notEmpty.Wait()

		// Check if we were awakened due to context cancellation
		select {
		case <-done:
			return nil, ctx.Err()
		default:
			// Continue normally
		}

		// If we were awakened but the queue is still empty, continue waiting
		if len(q.items) == 0 {
			continue
		}
	}

	// Find the first item that's ready to be processed
	now := time.Now()
	readyIndex := -1

	for i, item := range q.items {
		if item.ProcessAfter.IsZero() || !now.Before(item.ProcessAfter) {
			readyIndex = i
			break
		}
	}

	// If no items are ready, return error
	if readyIndex == -1 {
		return nil, ErrQueueEmpty
	}

	// Get the item and remove it from the queue
	item := q.items[readyIndex]
	// Remove the item by shifting elements
	q.items = append(q.items[:readyIndex], q.items[readyIndex+1:]...)

	return item, nil
}

// Peek returns the next item without removing it from the queue
func (q *InMemoryFIFO) Peek(ctx context.Context) (*QueueItem, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return nil, ErrQueueEmpty
	}

	// Find the first item that's ready to be processed
	now := time.Now()
	for _, item := range q.items {
		if item.ProcessAfter.IsZero() || !now.Before(item.ProcessAfter) {
			return item, nil
		}
	}

	// No items ready to be processed
	return nil, ErrQueueEmpty
}

// Size returns the current number of items in the queue
func (q *InMemoryFIFO) Size(ctx context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items), nil
}

// Clear removes all items from the queue
func (q *InMemoryFIFO) Clear(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = make([]*QueueItem, 0, q.maxSize)
	return nil
}

// Close releases any resources used by the queue
func (q *InMemoryFIFO) Close() error {
	return nil // No resources to release for in-memory queue
}

// RedisFIFO implements the Queue interface using Redis Lists
type RedisFIFO struct {
	client    redis.UniversalClient
	queueKey  string
	maxSize   int
	itemTTL   time.Duration
	keyExpiry time.Duration
}

// NewRedisQueue creates a new Redis-backed FIFO queue
func NewRedisQueue(config QueueConfig, client redis.UniversalClient) (*RedisFIFO, error) {
	if client == nil {
		return nil, errors.New("redis client is required")
	}

	if config.MaxSize <= 0 {
		config.MaxSize = 10000 // Default to 10000 items for distributed queue
	}

	if config.ItemTTL <= 0 {
		config.ItemTTL = 24 * time.Hour // Default to 24 hours
	}

	if config.RedisOptions == nil {
		config.RedisOptions = &RedisOptions{
			KeyPrefix: "queue:",
			KeyExpiry: 72 * time.Hour, // Default to 72 hours
		}
	}

	if config.RedisOptions.KeyPrefix == "" {
		config.RedisOptions.KeyPrefix = "queue:"
	}

	if config.RedisOptions.KeyExpiry <= 0 {
		config.RedisOptions.KeyExpiry = 72 * time.Hour
	}

	queueKey := config.RedisOptions.KeyPrefix + "fifo"

	q := &RedisFIFO{
		client:    client,
		queueKey:  queueKey,
		maxSize:   config.MaxSize,
		itemTTL:   config.ItemTTL,
		keyExpiry: config.RedisOptions.KeyExpiry,
	}

	utils.Info("Redis FIFO queue initialized", map[string]interface{}{
		"queue_key":  queueKey,
		"max_size":   config.MaxSize,
		"item_ttl":   config.ItemTTL.String(),
		"key_expiry": config.RedisOptions.KeyExpiry.String(),
	})

	return q, nil
}

// Enqueue adds an item to the queue
func (q *RedisFIFO) Enqueue(ctx context.Context, item *QueueItem) error {
	if item == nil {
		return errors.New("cannot enqueue nil item")
	}

	// Ensure the EnqueueTime is set
	if item.EnqueueTime.IsZero() {
		item.EnqueueTime = time.Now()
	}

	// Check queue size first
	size, err := q.client.LLen(ctx, q.queueKey).Result()
	if err != nil {
		utils.ErrorContext(ctx, err, "Failed to get queue size", map[string]interface{}{
			"queue_key": q.queueKey,
		})
		return fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	if size >= int64(q.maxSize) {
		return ErrQueueFull
	}

	// Serialize the item to JSON
	itemBytes, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("failed to serialize queue item: %w", err)
	}

	// Add to the end of the list (right side)
	err = q.client.RPush(ctx, q.queueKey, itemBytes).Err()
	if err != nil {
		utils.ErrorContext(ctx, err, "Failed to enqueue item", map[string]interface{}{
			"queue_key": q.queueKey,
		})
		return fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	// Ensure the queue key has an expiry
	q.client.Expire(ctx, q.queueKey, q.keyExpiry)

	return nil
}

// Dequeue removes and returns the next item from the queue
func (q *RedisFIFO) Dequeue(ctx context.Context) (*QueueItem, error) {
	// Use BLPOP with a timeout derived from the context
	deadline, ok := ctx.Deadline()
	timeout := 0 * time.Second // Default to no timeout
	if ok {
		timeout = time.Until(deadline)
		if timeout < 0 {
			timeout = 0
		}
	}

	// Try to get an item from the queue
	result, err := q.client.BLPop(ctx, timeout, q.queueKey).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, ErrQueueEmpty
		}
		utils.ErrorContext(ctx, err, "Failed to dequeue item", map[string]interface{}{
			"queue_key": q.queueKey,
		})
		return nil, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	// BLPOP returns an array where [0] is the key name and [1] is the value
	if len(result) < 2 {
		return nil, fmt.Errorf("unexpected result from BLPOP: %v", result)
	}

	// Deserialize the item
	var item QueueItem
	if err := json.Unmarshal([]byte(result[1]), &item); err != nil {
		return nil, fmt.Errorf("failed to deserialize queue item: %w", err)
	}

	// Check if the item has expired
	if time.Since(item.EnqueueTime) > q.itemTTL {
		utils.WarnContext(ctx, "Discarded expired queue item", map[string]interface{}{
			"item_id":      item.ID,
			"enqueue_time": item.EnqueueTime,
			"ttl":          q.itemTTL.String(),
		})
		// Try to get another item recursively
		return q.Dequeue(ctx)
	}

	// Check if this item should be processed later
	now := time.Now()
	if !item.ProcessAfter.IsZero() && now.Before(item.ProcessAfter) {
		// Put it back in the queue and try another item
		q.Enqueue(ctx, &item)
		return q.Dequeue(ctx)
	}

	return &item, nil
}

// Peek returns the next item without removing it from the queue
func (q *RedisFIFO) Peek(ctx context.Context) (*QueueItem, error) {
	// Get the first item in the list
	result, err := q.client.LIndex(ctx, q.queueKey, 0).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, ErrQueueEmpty
		}
		utils.ErrorContext(ctx, err, "Failed to peek at queue", map[string]interface{}{
			"queue_key": q.queueKey,
		})
		return nil, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	// Deserialize the item
	var item QueueItem
	if err := json.Unmarshal([]byte(result), &item); err != nil {
		return nil, fmt.Errorf("failed to deserialize queue item: %w", err)
	}

	// Check if the item has expired
	if time.Since(item.EnqueueTime) > q.itemTTL {
		// Item is expired, but we're just peeking so we don't remove it
		utils.WarnContext(ctx, "Peeked expired queue item", map[string]interface{}{
			"item_id":      item.ID,
			"enqueue_time": item.EnqueueTime,
			"ttl":          q.itemTTL.String(),
		})
	}

	// Check if this item should be processed later
	now := time.Now()
	if !item.ProcessAfter.IsZero() && now.Before(item.ProcessAfter) {
		// We don't have easy access to the second item in Redis without popping
		// Just return this item, but note it's not ready
		utils.InfoContext(ctx, "Peeked item not yet ready for processing", map[string]interface{}{
			"item_id":       item.ID,
			"process_after": item.ProcessAfter,
			"now":           now,
		})
	}

	return &item, nil
}

// Size returns the current number of items in the queue
func (q *RedisFIFO) Size(ctx context.Context) (int, error) {
	size, err := q.client.LLen(ctx, q.queueKey).Result()
	if err != nil {
		utils.ErrorContext(ctx, err, "Failed to get queue size", map[string]interface{}{
			"queue_key": q.queueKey,
		})
		return 0, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}
	return int(size), nil
}

// Clear removes all items from the queue
func (q *RedisFIFO) Clear(ctx context.Context) error {
	err := q.client.Del(ctx, q.queueKey).Err()
	if err != nil {
		utils.ErrorContext(ctx, err, "Failed to clear queue", map[string]interface{}{
			"queue_key": q.queueKey,
		})
		return fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}
	return nil
}

// Close releases any resources used by the queue
func (q *RedisFIFO) Close() error {
	return nil // Redis client should be closed by the caller
}

// NewQueue creates a new FIFO queue based on the provided configuration
func NewQueue(config QueueConfig, client redis.UniversalClient) (Queue, error) {
	switch config.Type {
	case InMemoryQueue:
		return NewInMemoryQueue(config)
	case RedisQueue:
		return NewRedisQueue(config, client)
	default:
		return nil, fmt.Errorf("unsupported queue type: %s", config.Type)
	}
}
