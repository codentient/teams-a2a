// Package store keeps the small amount of state the gateway needs between turns:
// which A2A context a chat maps to, the task currently running, the human-in-the-loop
// request awaiting an answer, delivery de-duplication, and per-chat turn locks.
package store

import (
	"context"
	"time"
)

type Session struct {
	// ContextID is the A2A context (kagent: the Session ID) of this chat.
	ContextID string `json:"contextId,omitempty"`
	// RunningTaskID is set while a turn is in flight so /cancel can reach it.
	RunningTaskID string `json:"runningTaskId,omitempty"`
	// Pending is set while the agent waits for the user (A2A INPUT_REQUIRED / AUTH_REQUIRED).
	Pending   *Pending  `json:"pending,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

const (
	PendingInput        = "input"         // plain INPUT_REQUIRED: the next message continues the task
	PendingToolApproval = "tool_approval" // kagent hitl/v1 tool_approval_request
	PendingAskUser      = "ask_user"      // kagent hitl/v1 ask_user_request
)

type Pending struct {
	Kind      string `json:"kind"`
	TaskID    string `json:"taskId"`
	ContextID string `json:"contextId"`
	// RequestID correlates an ask_user response.
	RequestID string     `json:"requestId,omitempty"`
	ToolIDs   []string   `json:"toolIds,omitempty"`
	Tools     []string   `json:"tools,omitempty"`
	Questions []Question `json:"questions,omitempty"`
	// Nonce binds a card submission to this exact pending request; card payloads are
	// otherwise untrusted user input.
	Nonce string `json:"nonce"`
	// CardRef is the channel's id for the prompt card, used to retire it once answered.
	CardRef     string `json:"cardRef,omitempty"`
	RequesterID string `json:"requesterId"`
	// Incomplete is set when the person was not shown the whole request (the card was too
	// large for the channel or failed to send). Such a request can be rejected, not approved.
	Incomplete bool      `json:"incomplete,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Question struct {
	Text     string   `json:"text"`
	Choices  []string `json:"choices,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
}

// Store is implemented by Memory (single replica) and Redis (multi replica).
type Store interface {
	GetSession(ctx context.Context, key string) (*Session, error)
	PutSession(ctx context.Context, key string, s *Session) error
	DeleteSession(ctx context.Context, key string) error
	// FirstDelivery records an inbound activity id and reports whether it was new.
	FirstDelivery(ctx context.Context, id string, ttl time.Duration) (bool, error)
	// Acquire takes a lock for owner if it is free or expired.
	Acquire(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
	// Release frees the lock only if owner still holds it.
	Release(ctx context.Context, key, owner string) error
	Ping(ctx context.Context) error
	Close() error
}
