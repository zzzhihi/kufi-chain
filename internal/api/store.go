// Package api provides storage utilities for the API layer
package api

import (
	"sync"
	"time"

	"github.com/fabric-payment-gateway/internal/receipt"
)

// ReceiptStorer defines the interface for receipt persistence.
// Implement this with PostgreSQL, Redis, etc. for production use.
type ReceiptStorer interface {
	Store(txID string, r *receipt.Receipt)
	Get(txID string) *receipt.Receipt
	Delete(txID string)
	Count() int
}

// IdempotencyStorer defines the interface for idempotency key persistence.
type IdempotencyStorer interface {
	Store(key string, r *receipt.Receipt)
	Get(key string) *receipt.Receipt
}

// NonceStorer defines the interface for nonce tracking.
type NonceStorer interface {
	Add(nonce string, timestamp time.Time)
	Exists(nonce string) bool
}

// Compile-time interface compliance checks
var (
	_ ReceiptStorer     = (*ReceiptStore)(nil)
	_ IdempotencyStorer = (*IdempotencyStore)(nil)
	_ NonceStorer       = (*NonceStore)(nil)
)

// ReceiptStore provides in-memory receipt storage
// In production, replace with persistent storage (PostgreSQL, Redis, etc.)
type ReceiptStore struct {
	mu       sync.RWMutex
	receipts map[string]*receipt.Receipt
}

// NewReceiptStore creates a new receipt store
func NewReceiptStore() *ReceiptStore {
	return &ReceiptStore{
		receipts: make(map[string]*receipt.Receipt),
	}
}

// Store stores a receipt by transaction ID
func (rs *ReceiptStore) Store(txID string, r *receipt.Receipt) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.receipts[txID] = r
}

// Get retrieves a receipt by transaction ID
func (rs *ReceiptStore) Get(txID string) *receipt.Receipt {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.receipts[txID]
}

// Delete removes a receipt by transaction ID
func (rs *ReceiptStore) Delete(txID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.receipts, txID)
}

// Count returns the number of stored receipts
func (rs *ReceiptStore) Count() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return len(rs.receipts)
}

// IdempotencyStore provides idempotency key storage with TTL
type IdempotencyStore struct {
	mu      sync.RWMutex
	entries map[string]*idempotencyEntry
	ttl     time.Duration
}

type idempotencyEntry struct {
	receipt   *receipt.Receipt
	expiresAt time.Time
}

// NewIdempotencyStore creates a new idempotency store
func NewIdempotencyStore(ttl time.Duration) *IdempotencyStore {
	store := &IdempotencyStore{
		entries: make(map[string]*idempotencyEntry),
		ttl:     ttl,
	}
	
	// Start cleanup goroutine
	go store.cleanup()
	
	return store
}

// Store stores a receipt by idempotency key
func (is *IdempotencyStore) Store(key string, r *receipt.Receipt) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.entries[key] = &idempotencyEntry{
		receipt:   r,
		expiresAt: time.Now().Add(is.ttl),
	}
}

// Get retrieves a receipt by idempotency key
func (is *IdempotencyStore) Get(key string) *receipt.Receipt {
	is.mu.RLock()
	defer is.mu.RUnlock()
	
	entry, ok := is.entries[key]
	if !ok {
		return nil
	}
	
	if time.Now().After(entry.expiresAt) {
		return nil
	}
	
	return entry.receipt
}

// cleanup removes expired entries periodically
func (is *IdempotencyStore) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		is.mu.Lock()
		now := time.Now()
		for key, entry := range is.entries {
			if now.After(entry.expiresAt) {
				delete(is.entries, key)
			}
		}
		is.mu.Unlock()
	}
}

// NonceStore provides nonce tracking for replay protection
type NonceStore struct {
	mu      sync.RWMutex
	nonces  map[string]time.Time
	window  time.Duration
}

// NewNonceStore creates a new nonce store
func NewNonceStore(window time.Duration) *NonceStore {
	store := &NonceStore{
		nonces: make(map[string]time.Time),
		window: window,
	}
	
	// Start cleanup goroutine
	go store.cleanup()
	
	return store
}

// Add adds a nonce with timestamp
func (ns *NonceStore) Add(nonce string, timestamp time.Time) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.nonces[nonce] = timestamp
}

// Exists checks if a nonce exists and is within window
func (ns *NonceStore) Exists(nonce string) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	
	timestamp, ok := ns.nonces[nonce]
	if !ok {
		return false
	}
	
	// Check if within window
	return time.Since(timestamp) < ns.window
}

// cleanup removes expired nonces periodically
func (ns *NonceStore) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		ns.mu.Lock()
		now := time.Now()
		for nonce, timestamp := range ns.nonces {
			if now.Sub(timestamp) > ns.window {
				delete(ns.nonces, nonce)
			}
		}
		ns.mu.Unlock()
	}
}

// WorkerPool manages concurrent transaction processing
type WorkerPool struct {
	size      int
	queue     chan Job
	quit      chan struct{}
	wg        sync.WaitGroup
}

// Job represents a work item
type Job struct {
	Execute func() error
	Result  chan error
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(size, queueSize int) *WorkerPool {
	wp := &WorkerPool{
		size:  size,
		queue: make(chan Job, queueSize),
		quit:  make(chan struct{}),
	}
	
	wp.Start()
	return wp
}

// Start starts the worker pool
func (wp *WorkerPool) Start() {
	for i := 0; i < wp.size; i++ {
		wp.wg.Add(1)
		go wp.worker()
	}
}

// worker processes jobs from the queue
func (wp *WorkerPool) worker() {
	defer wp.wg.Done()
	
	for {
		select {
		case job := <-wp.queue:
			err := job.Execute()
			if job.Result != nil {
				job.Result <- err
			}
		case <-wp.quit:
			return
		}
	}
}

// Submit submits a job to the pool
func (wp *WorkerPool) Submit(job Job) bool {
	select {
	case wp.queue <- job:
		return true
	default:
		// Queue full
		return false
	}
}

// Stop stops the worker pool
func (wp *WorkerPool) Stop() {
	close(wp.quit)
	wp.wg.Wait()
}

// QueueLength returns the current queue length
func (wp *WorkerPool) QueueLength() int {
	return len(wp.queue)
}

// Deprecated: WorkerPool is not used by SubmitTransfer.
// Kept for backward compatibility but may be removed in a future release.
