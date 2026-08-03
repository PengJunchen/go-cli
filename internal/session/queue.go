package session

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// QueueType identifies one of the three pending-submission queues.
type QueueType int

const (
	// QueueSteering holds submissions injected mid-turn to change agent
	// direction. They are intended to interrupt the current turn.
	QueueSteering QueueType = iota
	// QueueFollowUp holds submissions queued for after the current turn
	// completes. They run automatically once the turn finishes.
	QueueFollowUp
	// QueueNextRun holds submissions queued for the next explicit run.
	// They wait until the user explicitly starts a new run.
	QueueNextRun
)

// String returns a stable, lowercase identifier for the queue type.
func (q QueueType) String() string {
	switch q {
	case QueueSteering:
		return "steering"
	case QueueFollowUp:
		return "follow_up"
	case QueueNextRun:
		return "next_run"
	default:
		return "unknown"
	}
}

// QueueMode controls how many items are processed per dequeue operation.
type QueueMode int

const (
	// QueueModeOneAtATime processes a single item per dequeue.
	QueueModeOneAtATime QueueMode = iota
	// QueueModeAll processes all available items at once via Drain.
	QueueModeAll
)

// QueuedSubmission is a single submission waiting in one of the three queues.
type QueuedSubmission struct {
	ID          string    `json:"id"`
	Content     string    `json:"content"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
}

// SubmissionQueue manages three types of pending submissions:
//   - Steering: injected mid-turn to change agent direction
//   - FollowUp: queued for after current turn completes
//   - NextRun: queued for the next explicit run
type SubmissionQueue interface {
	// Enqueue adds a submission to the specified queue.
	Enqueue(qType QueueType, submission QueuedSubmission) error
	// Dequeue removes and returns the front item of the specified queue
	// (FIFO). Returns ok=false when the queue is empty.
	Dequeue(qType QueueType) (QueuedSubmission, bool)
	// Peek returns the front item without removing it. Returns ok=false
	// when the queue is empty.
	Peek(qType QueueType) (QueuedSubmission, bool)
	// Len returns the number of items in the specified queue.
	Len(qType QueueType) int
	// Drain returns all items from the specified queue and clears it.
	Drain(qType QueueType) []QueuedSubmission
	// Abort clears the specified queue and returns the number of items
	// that were discarded.
	Abort(qType QueueType) int
}

// DefaultSubmissionQueue is the default in-memory SubmissionQueue. It uses
// three FIFO slices guarded by a single mutex. It is safe for concurrent use.
type DefaultSubmissionQueue struct {
	mu      sync.Mutex
	queues  [3][]QueuedSubmission
	mode    QueueMode
	counter uint64
}

// Compile-time assertion that DefaultSubmissionQueue satisfies SubmissionQueue.
var _ SubmissionQueue = (*DefaultSubmissionQueue)(nil)

// NewDefaultSubmissionQueue returns an empty in-memory submission queue using
// the one-at-a-time dequeue mode.
func NewDefaultSubmissionQueue() *DefaultSubmissionQueue {
	return &DefaultSubmissionQueue{mode: QueueModeOneAtATime}
}

// NewDefaultSubmissionQueueWithMode returns an empty in-memory submission queue
// configured with the given processing mode.
func NewDefaultSubmissionQueueWithMode(mode QueueMode) *DefaultSubmissionQueue {
	return &DefaultSubmissionQueue{mode: mode}
}

// SetMode updates the processing mode of the queue.
func (q *DefaultSubmissionQueue) SetMode(mode QueueMode) {
	q.mu.Lock()
	q.mode = mode
	q.mu.Unlock()
	slog.Debug("session.queue.set_mode", "mode", mode)
}

// Mode returns the current processing mode.
func (q *DefaultSubmissionQueue) Mode() QueueMode {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.mode
}

func (q *DefaultSubmissionQueue) validateType(qType QueueType) error {
	if qType < QueueSteering || qType > QueueNextRun {
		return fmt.Errorf("session: invalid queue type %d", qType)
	}
	return nil
}

// Enqueue appends a submission to the tail of the specified queue. A submission
// with an empty ID receives an auto-generated one.
func (q *DefaultSubmissionQueue) Enqueue(qType QueueType, submission QueuedSubmission) error {
	if err := q.validateType(qType); err != nil {
		slog.Warn("session.queue.enqueue", "error_type", "invalid_type", "queue_type", qType)
		return err
	}
	q.mu.Lock()
	if submission.ID == "" {
		q.counter++
		submission.ID = fmt.Sprintf("sub-%d", q.counter)
	}
	if submission.EnqueuedAt.IsZero() {
		submission.EnqueuedAt = time.Now().UTC()
	}
	q.queues[qType] = append(q.queues[qType], submission)
	q.mu.Unlock()

	slog.Info("session.queue.enqueue",
		"queue_type", qType.String(),
		"submission_id", submission.ID,
		"queue_len", q.Len(qType),
	)
	return nil
}

// Dequeue removes and returns the front item of the specified queue (FIFO).
// Returns ok=false when the queue is empty.
func (q *DefaultSubmissionQueue) Dequeue(qType QueueType) (QueuedSubmission, bool) {
	if err := q.validateType(qType); err != nil {
		slog.Warn("session.queue.dequeue", "error_type", "invalid_type", "queue_type", qType)
		return QueuedSubmission{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queues[qType]) == 0 {
		return QueuedSubmission{}, false
	}
	item := q.queues[qType][0]
	q.queues[qType] = q.queues[qType][1:]
	slog.Info("session.queue.dequeue",
		"queue_type", qType.String(),
		"submission_id", item.ID,
		"remaining", len(q.queues[qType]),
	)
	return item, true
}

// Peek returns the front item without removing it. Returns ok=false when the
// queue is empty.
func (q *DefaultSubmissionQueue) Peek(qType QueueType) (QueuedSubmission, bool) {
	if err := q.validateType(qType); err != nil {
		return QueuedSubmission{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queues[qType]) == 0 {
		return QueuedSubmission{}, false
	}
	return q.queues[qType][0], true
}

// Len returns the number of items in the specified queue.
func (q *DefaultSubmissionQueue) Len(qType QueueType) int {
	if err := q.validateType(qType); err != nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queues[qType])
}

// Drain returns all items from the specified queue and clears it.
func (q *DefaultSubmissionQueue) Drain(qType QueueType) []QueuedSubmission {
	if err := q.validateType(qType); err != nil {
		return nil
	}
	q.mu.Lock()
	items := q.queues[qType]
	q.queues[qType] = nil
	q.mu.Unlock()

	slog.Info("session.queue.drain",
		"queue_type", qType.String(),
		"drained_count", len(items),
	)
	return items
}

// Abort clears the specified queue and returns the number of items that were
// discarded.
func (q *DefaultSubmissionQueue) Abort(qType QueueType) int {
	if err := q.validateType(qType); err != nil {
		return 0
	}
	q.mu.Lock()
	count := len(q.queues[qType])
	q.queues[qType] = nil
	q.mu.Unlock()

	slog.Info("session.queue.abort",
		"queue_type", qType.String(),
		"aborted_count", count,
	)
	return count
}

// TotalLen returns the total number of pending items across all three queues.
func (q *DefaultSubmissionQueue) TotalLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	total := 0
	for i := range q.queues {
		total += len(q.queues[i])
	}
	return total
}
