# LLM Request Balancer (WIP)

A high-performance, scalable system for managing and queing rate-limited requests to Large Language Model (LLM) APIs. This service implements token bucket rate limiting with intelligent queuing to ensure optimal utilization of API rate limits while preventing overload.

[![Go Version](https://img.shields.io/badge/Go-1.24-blue.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

## Overview

The LLM Load Balancer is designed to serve as a middleware between your applications and various LLM providers (like OpenAI, Anthropic). It implements sophisticated rate limiting based on token consumption and intelligent request queuing to ensure:

- LLM API rate limits are respected
- Requests exceeding rate limits are queued rather than rejected
- Fair request scheduling based on arrival time (FIFO)
- Efficient utilization of available token capacity
- Support for multiple LLM providers through a unified API

## Key Features

- **Token-Based Rate Limiting**: Implements the token bucket algorithm to enforce per-model token limits
- **Intelligent Request Queuing**: Queues requests when rate limits are reached, processing them automatically when tokens become available
- **Distributed Deployment Support**: Optional Redis backend for shared rate limiting and queuing state across multiple instances
- **Multiple Provider Support**: Unified interface for OpenAI, Anthropic, and other LLM providers
- **Asynchronous Processing**: Non-blocking request handling for high throughput
- **Configurable Concurrency**: Control the number of concurrent requests to LLM providers
- **Health Monitoring**: Endpoints for monitoring service health and queue status
- **Long Polling**: Clients can wait for queued requests with configurable timeouts

## Architecture```
                   +------------------------+
                   |     API Gateway        |
                   |  (SSL termination,     |
                   |   authentication,      |
                   |   basic throttling)    |
                   +-----------+------------+
                               │
                               ▼
                   +-------------------------+
                   |   Golang HTTP Server    |<------+
                   |   with Request Handlers |       |
                   |                         |       |
                   +-----------+-------------+       |
                               │                     |
             +-----------------+-----------------+   |
             |                                   |   |
             ▼                                   ▼   |
  +----------------------+         +----------------------+
  |  Token Bucket & FIFO |         |  Worker Pool with    |
  |  Queuing (In-memory  |<------->|  Asynchronous LLM    | 
  |  or Redis-backed)    |  Shared |  Provider API Callers| 
  +----------------------+  state  +----------------------+
             │                                   │
             +---------------+-------------------+
                             │
                             ▼
                   +------------------------+
                   |   LLM Provider APIs    |
                   |  (OpenAI, Anthropic)   |
                   +------------------------+
```

## Getting Started

### Prerequisites

- Go 1.24+
- Redis (optional, for distributed mode)

### Installation

1. Clone the repository:
   ```bash
   git clone https://github.com/pdmimpulse/LLM-Load-Balancer.git
   cd LLM-Load-Balancer
   ```

2. Install dependencies:
   ```bash
   go mod download
   ```

3. Build the project:
   ```bash
   make build
   ```

### Configuration

Configure the system by editing `internal/config/config.json`:

```json
{
  "server": {
    "port": 8080,
    "readTimeout": 30,
    "writeTimeout": 120
  },
  "rateLimiter": {
    "useRedis": false,
    "tokensPerSecond": 100,
    "bucketSize": 500,
    "keyPrefix": "ratelimit:"
  },
  "redis": {
    "addr": "localhost:6379",
    "password": "",
    "db": 0,
    "poolSize": 10
  },
  "llmProvider": {
    "defaultProvider": "openai",
    "defaultModel": "gpt-4",
    "requestTimeout": 60,
    "apiKeys": {
      "openai": "your-openai-api-key",
      "anthropic": "your-anthropic-api-key"
    }
  },
  "logging": {
    "level": "info",
    "pretty": true
  }
}
```

### Running the Service

#### Standalone Mode (In-Memory)

```bash
./bin/server --config=internal/config/config.json
```

#### Distributed Mode (Redis-Backed)

1. Ensure Redis is running and accessible
2. Set `"useRedis": true` in the configuration file
3. Run the server:
   ```bash
   ./bin/server --config=internal/config/config.json
   ```

## Usage Examples

The system uses a single endpoint for all completion requests.

### Making a Chat Completion Request

```bash
curl -X POST http://localhost:8080/api/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "openai",
    "model": "gpt-4",
    "payload": {
      "messages": [
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "What is the capital of France?"}
      ]
    }
  }'
```

### Checking Service Health

```bash
curl http://localhost:8080/health
```

## Development

### Project Structure

```
├── cmd/                  # Application entrypoints
│   ├── server/           # Main server application
│   ├── server_launcher/  # Launcher with UI
│   └── test/             # Test utilities
├── internal/             # Private application code
│   ├── config/           # Configuration management
│   ├── handler/          # HTTP request handlers
│   ├── limiter/          # Rate limiting implementation
│   ├── llm/              # LLM provider interfaces
│   ├── proxy/            # Proxy utilities
│   ├── queue/            # Request queuing
│   └── utils/            # Shared utilities
├── pkg/                  # Public libraries that can be used by external applications
├── deployments/          # Deployment configurations
└── docs/                 # Documentation
```

## Contributing

To contribute to the LLM Request Balancer project, please contact us at y.lu@pdm-solutions.com to be added as a collaborator to the repository.

## License

This project is licensed under the MIT License - see the LICENSE file for details.

## Acknowledgements

- Go Redis for the Redis client
- Viper for configuration management
- ZeroLog for structured logging 

