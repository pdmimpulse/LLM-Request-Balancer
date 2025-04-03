// Package queue provides queue-related functionality for the LLM Load Balancer.
// This file implements a thread-safe registry for managing response channels.
// It allows the system to maintain references to Go channels that cannot be
// serialized in Redis or other storage, enabling asynchronous communication
// between different parts of the application.

package queue

import (
	"sync"

	"github.com/pdmimpulse/LLM-Request-Balancer/internal/llm"
)

// ChannelRegistry maintains a map of request IDs to response channels
// This allows us to maintain channel references that can't be serialized
type ChannelRegistry struct {
	channels map[string]chan<- *llm.Response
	mu       sync.RWMutex
}

// NewChannelRegistry creates a new channel registry
func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		channels: make(map[string]chan<- *llm.Response),
	}
}

// Register adds a response channel to the registry
func (r *ChannelRegistry) Register(requestID string, ch chan<- *llm.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[requestID] = ch
}

// Get retrieves a response channel from the registry
func (r *ChannelRegistry) Get(requestID string) chan<- *llm.Response {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.channels[requestID]
}

// Remove deletes a response channel from the registry
func (r *ChannelRegistry) Remove(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.channels, requestID)
}

// Clear removes all channels from the registry
func (r *ChannelRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = make(map[string]chan<- *llm.Response)
}
