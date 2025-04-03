// Package redisclient provides a Redis client wrapper for the LLM Load Balancer.
//
// This file implements a Redis client wrapper that provides simplified access
// to Redis operations with additional error handling and connection management.
// It supports common Redis operations like Get, Set, List operations, and Lua script
// evaluation, making it easier to interact with Redis throughout the application.

package redisclient

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pdmimpulse/LLM-Request-Balancer/internal/utils"
)

// Client wraps a Redis client with additional functionality
type Client struct {
	rdb *redis.Client
}

// Config contains Redis connection settings
type Config struct {
	Addr     string
	Password string
	DB       int
	PoolSize int
}

// NewClient creates a new Redis client
func NewClient(cfg Config) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: 10,
		MaxRetries:   5,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolTimeout:  4 * time.Second,
	})

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	utils.Info("Connected to Redis", map[string]interface{}{
		"addr": cfg.Addr,
		"db":   cfg.DB,
	})

	return &Client{rdb: rdb}, nil
}

// Close closes the Redis client
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Get retrieves a key's value
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	val, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil // Key doesn't exist
	}
	return val, err
}

// Set sets a key's value with optional expiration
func (c *Client) Set(ctx context.Context, key, value string, expiration time.Duration) error {
	return c.rdb.Set(ctx, key, value, expiration).Err()
}

// Incr increments a key's value
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Incr(ctx, key).Result()
}

// Decr decrements a key's value
func (c *Client) Decr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Decr(ctx, key).Result()
}

// DecrBy decrements a key's value by the given amount
func (c *Client) DecrBy(ctx context.Context, key string, value int64) (int64, error) {
	return c.rdb.DecrBy(ctx, key, value).Result()
}

// IncrBy increments a key's value by the given amount
func (c *Client) IncrBy(ctx context.Context, key string, value int64) (int64, error) {
	return c.rdb.IncrBy(ctx, key, value).Result()
}

// Del deletes a key
func (c *Client) Del(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, key).Err()
}

// Expire sets a key's expiration
func (c *Client) Expire(ctx context.Context, key string, expiration time.Duration) error {
	return c.rdb.Expire(ctx, key, expiration).Err()
}

// LPush adds values to the left of a list
func (c *Client) LPush(ctx context.Context, key string, values ...interface{}) (int64, error) {
	return c.rdb.LPush(ctx, key, values...).Result()
}

// RPop removes and returns the last element of a list
func (c *Client) RPop(ctx context.Context, key string) (string, error) {
	val, err := c.rdb.RPop(ctx, key).Result()
	if err == redis.Nil {
		return "", nil // List is empty
	}
	return val, err
}

// BRPop blocks until an element is popped from one of the given lists or timeout
func (c *Client) BRPop(ctx context.Context, timeout time.Duration, keys ...string) ([]string, error) {
	result, err := c.rdb.BRPop(ctx, timeout, keys...).Result()
	if err == redis.Nil {
		return nil, nil // Timeout reached without elements
	}
	return result, err
}

// LLen returns the length of a list
func (c *Client) LLen(ctx context.Context, key string) (int64, error) {
	return c.rdb.LLen(ctx, key).Result()
}

// Eval evaluates a Lua script
func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error) {
	return c.rdb.Eval(ctx, script, keys, args...).Result()
}

// Pipeline returns a new Redis pipeline
func (c *Client) Pipeline() redis.Pipeliner {
	return c.rdb.Pipeline()
}

// WithRedisClient provides a dependency-injection friendly way to use the Redis client
// It runs the given function with a Redis client and handles any connection errors
func WithRedisClient(cfg Config, fn func(*Client) error) error {
	client, err := NewClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	return fn(client)
}
