// Package memory provides cross-session memory storage and retrieval.
package memory

import (
	"context"
	"time"
)

// Memory represents a cross-session memory entry.
type Memory struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	Category  string    `json:"category"`            // preference | fact | decision | convention
	Source    string    `json:"source"`              // auto | manual
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Relevance float64   `json:"relevance,omitempty"` // 0-1, filled during retrieval
}

// MemoryStore manages memory storage and retrieval.
type MemoryStore interface {
	Add(ctx context.Context, mem Memory) error
	Get(ctx context.Context, id string) (Memory, error)
	List(ctx context.Context) ([]Memory, error)
	Delete(ctx context.Context, id string) error
	Search(ctx context.Context, query string, limit int) ([]Memory, error)
	Close() error
}

// MemoryRetriever wraps search for use in prompt injection, where only
// relevance-ranked retrieval is needed.
type MemoryRetriever interface {
	RetrieveRelevant(ctx context.Context, query string, limit int) ([]Memory, error)
}
