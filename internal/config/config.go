// Package config provides configuration loading, validation, and access for the LLM Load Balancer.
//
// This file contains:
// 1. Configuration structure definitions that map to JSON/YAML configuration files
// 2. Loading logic using Viper to read from files and environment variables
// 3. Default value settings for all configuration parameters
// 4. Validation logic to ensure required settings are provided
//
// The configuration structure supports settings for:
// - Server parameters (port, timeouts, health checks)
// - Redis connection details
// - Rate limiting settings with per-model token limits
// - LLM provider API configuration
// - Logging preferences
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config represents the application configuration
type Config struct {
	// Server settings
	Server ServerConfig `mapstructure:"server"`

	// Redis configuration
	Redis RedisConfig `mapstructure:"redis"`

	// Rate limiting configuration
	RateLimiter RateLimiterConfig `mapstructure:"rateLimiter"`

	// LLM provider configuration
	LLMProvider LLMProviderConfig `mapstructure:"llmProvider"`

	// Logging configuration
	Logging LoggingConfig `mapstructure:"logging"`
}

// ServerConfig contains server-specific settings
type ServerConfig struct {
	Port                string        `mapstructure:"port"`
	HealthCheckPath     string        `mapstructure:"healthCheckPath"`
	HealthCheckInterval time.Duration `mapstructure:"healthCheckInterval"`
	ReadTimeout         time.Duration `mapstructure:"readTimeout"`
	WriteTimeout        time.Duration `mapstructure:"writeTimeout"`
	IdleTimeout         time.Duration `mapstructure:"idleTimeout"`
	ShutdownTimeout     time.Duration `mapstructure:"shutdownTimeout"`
}

// RedisConfig contains Redis connection settings
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"poolSize"`
}

// RateLimiterConfig contains rate limiting settings
type RateLimiterConfig struct {
	Enabled         bool          `mapstructure:"enabled"`
	UseRedis        bool          `mapstructure:"useRedis"`
	TokensPerSecond float64       `mapstructure:"tokensPerSecond"`
	BucketSize      int           `mapstructure:"bucketSize"`
	KeyPrefix       string        `mapstructure:"keyPrefix"`
	WaitTimeout     time.Duration `mapstructure:"waitTimeout"`

	// Per-model rate limits (model name -> tokens per second)
	ModelLimits map[string]float64 `mapstructure:"modelLimits"`
}

// LLMProviderConfig contains settings for multiple LLM providers
type LLMProviderConfig struct {
	// Providers is a map of provider name to its configuration
	Providers map[string]ProviderSettings `mapstructure:"providers"`

	// DefaultProvider is the provider to use when none is specified
	DefaultProvider string `mapstructure:"defaultProvider"`

	// DefaultModel is the model to use when none is specified
	DefaultModel string `mapstructure:"defaultModel"`
}

// ProviderSettings contains settings for a specific LLM provider
type ProviderSettings struct {
	BaseURL         string        `mapstructure:"baseURL"`
	APIKey          string        `mapstructure:"apiKey"`
	RequestTimeout  time.Duration `mapstructure:"requestTimeout"`
	MaxRetries      int           `mapstructure:"maxRetries"`
	RetryBackoffMin time.Duration `mapstructure:"retryBackoffMin"`
	RetryBackoffMax time.Duration `mapstructure:"retryBackoffMax"`
	Models          []ModelInfo   `mapstructure:"models"`
}

// ModelInfo contains information about a specific model
type ModelInfo struct {
	Name        string `mapstructure:"name"`
	TokenCostFn string `mapstructure:"tokenCostFn"` // Name of function to calculate token cost
}

// LoggingConfig contains logging settings
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Pretty bool   `mapstructure:"pretty"`
}

// LoadConfig loads the configuration from the specified file
func LoadConfig(configPath string) (*Config, error) {
	v := viper.New()

	// Set defaults
	setDefaults(v)

	// Configure Viper for environment variables
	v.SetEnvPrefix("LLM_BALANCER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// If configPath is provided, use it
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		// Otherwise, look for config in default locations
		v.SetConfigName("config")
		v.SetConfigType("json")
		v.AddConfigPath(".")
		v.AddConfigPath("./config")
		v.AddConfigPath("./internal/config")
	}

	// Read the config file
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Unmarshal config
	var config Config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Validate config
	if err := validateConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// setDefaults sets default values for configuration
func setDefaults(v *viper.Viper) {
	// Server defaults
	v.SetDefault("server.port", ":8080")
	v.SetDefault("server.healthCheckPath", "/health")
	v.SetDefault("server.healthCheckInterval", "5s")
	v.SetDefault("server.readTimeout", "30s")
	v.SetDefault("server.writeTimeout", "60s")
	v.SetDefault("server.idleTimeout", "120s")
	v.SetDefault("server.shutdownTimeout", "30s")

	// Redis defaults
	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.poolSize", 10)

	// Rate limiter defaults
	v.SetDefault("rateLimiter.enabled", true)
	v.SetDefault("rateLimiter.useRedis", true)
	v.SetDefault("rateLimiter.tokensPerSecond", 100.0)
	v.SetDefault("rateLimiter.bucketSize", 1000)
	v.SetDefault("rateLimiter.keyPrefix", "llm-rate-limit:")
	v.SetDefault("rateLimiter.waitTimeout", "30s")

	// LLM provider defaults
	v.SetDefault("llmProvider.requestTimeout", "60s")
	v.SetDefault("llmProvider.maxRetries", 3)
	v.SetDefault("llmProvider.retryBackoffMin", "100ms")
	v.SetDefault("llmProvider.retryBackoffMax", "10s")

	// Logging defaults
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.pretty", false)
}

// validateConfig validates the loaded configuration
func validateConfig(cfg *Config) error {
	// Validate server config
	if cfg.Server.Port == "" {
		return fmt.Errorf("server port must be specified")
	}

	// Validate Redis config if rate limiter uses Redis
	if cfg.RateLimiter.UseRedis && cfg.Redis.Addr == "" {
		return fmt.Errorf("redis address must be specified when using Redis rate limiter")
	}

	// Validate LLM provider config
	if cfg.LLMProvider.DefaultProvider == "" {
		return fmt.Errorf("LLM provider default provider must be specified")
	}
	if cfg.LLMProvider.DefaultModel == "" {
		return fmt.Errorf("LLM provider default model must be specified")
	}

	// Additional validation as needed

	return nil
}
