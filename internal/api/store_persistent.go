// Package api provides file-based persistent storage implementations
package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fabric-payment-gateway/internal/receipt"
)

// FileReceiptStore provides file-based persistent receipt storage
type FileReceiptStore struct {
	mu      sync.RWMutex
	cache   map[string]*receipt.Receipt
	dataDir string
}

// NewFileReceiptStore creates a persistent receipt store backed by JSON files
func NewFileReceiptStore(dataDir string) (*FileReceiptStore, error) {
	receiptDir := filepath.Join(dataDir, "receipts")
	if err := os.MkdirAll(receiptDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create receipt directory: %w", err)
	}

	store := &FileReceiptStore{
		cache:   make(map[string]*receipt.Receipt),
		dataDir: receiptDir,
	}

	// Load existing receipts from disk into cache
	if err := store.loadAll(); err != nil {
		return nil, fmt.Errorf("failed to load existing receipts: %w", err)
	}

	return store, nil
}

func (fs *FileReceiptStore) loadAll() error {
	entries, err := os.ReadDir(fs.dataDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(fs.dataDir, entry.Name()))
		if err != nil {
			continue // skip unreadable files
		}

		var r receipt.Receipt
		if err := json.Unmarshal(data, &r); err != nil {
			continue // skip corrupt files
		}

		fs.cache[r.TxID] = &r
	}

	return nil
}

// Store stores a receipt by transaction ID (writes to disk + cache)
func (fs *FileReceiptStore) Store(txID string, r *receipt.Receipt) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.cache[txID] = r

	// Write to disk asynchronously (best effort)
	go func() {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return
		}
		filePath := filepath.Join(fs.dataDir, txID+".json")
		_ = os.WriteFile(filePath, data, 0644)
	}()
}

// Get retrieves a receipt by transaction ID
func (fs *FileReceiptStore) Get(txID string) *receipt.Receipt {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.cache[txID]
}

// Delete removes a receipt by transaction ID
func (fs *FileReceiptStore) Delete(txID string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	delete(fs.cache, txID)

	// Remove from disk
	filePath := filepath.Join(fs.dataDir, txID+".json")
	_ = os.Remove(filePath)
}

// Count returns the number of stored receipts
func (fs *FileReceiptStore) Count() int {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return len(fs.cache)
}

// FileIdempotencyStore provides file-based persistent idempotency key storage
type FileIdempotencyStore struct {
	mu      sync.RWMutex
	entries map[string]*persistentIdempotencyEntry
	ttl     time.Duration
	dataDir string
}

type persistentIdempotencyEntry struct {
	Receipt   *receipt.Receipt `json:"receipt"`
	ExpiresAt time.Time        `json:"expires_at"`
}

// NewFileIdempotencyStore creates a persistent idempotency store
func NewFileIdempotencyStore(ttl time.Duration, dataDir string) (*FileIdempotencyStore, error) {
	idemDir := filepath.Join(dataDir, "idempotency")
	if err := os.MkdirAll(idemDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create idempotency directory: %w", err)
	}

	store := &FileIdempotencyStore{
		entries: make(map[string]*persistentIdempotencyEntry),
		ttl:     ttl,
		dataDir: idemDir,
	}

	// Load existing entries
	if err := store.loadAll(); err != nil {
		return nil, fmt.Errorf("failed to load idempotency entries: %w", err)
	}

	// Start cleanup goroutine
	go store.cleanup()

	return store, nil
}

func (fis *FileIdempotencyStore) loadAll() error {
	entries, err := os.ReadDir(fis.dataDir)
	if err != nil {
		return err
	}

	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(fis.dataDir, entry.Name()))
		if err != nil {
			continue
		}

		var e persistentIdempotencyEntry
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}

		// Skip expired entries
		if now.After(e.ExpiresAt) {
			_ = os.Remove(filepath.Join(fis.dataDir, entry.Name()))
			continue
		}

		key := entry.Name()[:len(entry.Name())-5] // strip .json
		fis.entries[key] = &e
	}

	return nil
}

// Store stores a receipt by idempotency key
func (fis *FileIdempotencyStore) Store(key string, r *receipt.Receipt) {
	fis.mu.Lock()
	defer fis.mu.Unlock()

	entry := &persistentIdempotencyEntry{
		Receipt:   r,
		ExpiresAt: time.Now().Add(fis.ttl),
	}
	fis.entries[key] = entry

	// Write to disk
	go func() {
		data, err := json.Marshal(entry)
		if err != nil {
			return
		}
		filePath := filepath.Join(fis.dataDir, key+".json")
		_ = os.WriteFile(filePath, data, 0644)
	}()
}

// Get retrieves a receipt by idempotency key
func (fis *FileIdempotencyStore) Get(key string) *receipt.Receipt {
	fis.mu.RLock()
	defer fis.mu.RUnlock()

	entry, ok := fis.entries[key]
	if !ok {
		return nil
	}

	if time.Now().After(entry.ExpiresAt) {
		return nil
	}

	return entry.Receipt
}

// cleanup removes expired entries periodically
func (fis *FileIdempotencyStore) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		fis.mu.Lock()
		now := time.Now()
		for key, entry := range fis.entries {
			if now.After(entry.ExpiresAt) {
				delete(fis.entries, key)
				_ = os.Remove(filepath.Join(fis.dataDir, key+".json"))
			}
		}
		fis.mu.Unlock()
	}
}

// FileNonceStore provides file-based persistent nonce storage
type FileNonceStore struct {
	mu      sync.RWMutex
	nonces  map[string]time.Time
	window  time.Duration
	dataDir string
}

// NewFileNonceStore creates a persistent nonce store
func NewFileNonceStore(window time.Duration, dataDir string) (*FileNonceStore, error) {
	nonceDir := filepath.Join(dataDir, "nonces")
	if err := os.MkdirAll(nonceDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create nonce directory: %w", err)
	}

	store := &FileNonceStore{
		nonces:  make(map[string]time.Time),
		window:  window,
		dataDir: nonceDir,
	}

	// Load from single nonces file
	if err := store.loadAll(); err != nil {
		// Not fatal — start fresh
		_ = err
	}

	// Start cleanup goroutine
	go store.cleanup()

	return store, nil
}

type nonceFileData struct {
	Nonces map[string]time.Time `json:"nonces"`
}

func (fns *FileNonceStore) loadAll() error {
	data, err := os.ReadFile(filepath.Join(fns.dataDir, "nonces.json"))
	if err != nil {
		return err
	}

	var nfd nonceFileData
	if err := json.Unmarshal(data, &nfd); err != nil {
		return err
	}

	now := time.Now()
	for nonce, ts := range nfd.Nonces {
		if now.Sub(ts) < fns.window {
			fns.nonces[nonce] = ts
		}
	}

	return nil
}

func (fns *FileNonceStore) persist() {
	nfd := nonceFileData{Nonces: fns.nonces}
	data, err := json.Marshal(nfd)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(fns.dataDir, "nonces.json"), data, 0644)
}

// Add adds a nonce with timestamp
func (fns *FileNonceStore) Add(nonce string, timestamp time.Time) {
	fns.mu.Lock()
	defer fns.mu.Unlock()
	fns.nonces[nonce] = timestamp

	// Persist periodically (every 100 nonces)
	if len(fns.nonces)%100 == 0 {
		go func() {
			fns.mu.RLock()
			defer fns.mu.RUnlock()
			fns.persist()
		}()
	}
}

// Exists checks if a nonce exists and is within window
func (fns *FileNonceStore) Exists(nonce string) bool {
	fns.mu.RLock()
	defer fns.mu.RUnlock()

	timestamp, ok := fns.nonces[nonce]
	if !ok {
		return false
	}

	return time.Since(timestamp) < fns.window
}

// cleanup removes expired nonces periodically and persists to disk
func (fns *FileNonceStore) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		fns.mu.Lock()
		now := time.Now()
		for nonce, timestamp := range fns.nonces {
			if now.Sub(timestamp) > fns.window {
				delete(fns.nonces, nonce)
			}
		}
		fns.persist()
		fns.mu.Unlock()
	}
}

// Compile-time interface compliance checks for persistent stores
var (
	_ ReceiptStorer     = (*FileReceiptStore)(nil)
	_ IdempotencyStorer = (*FileIdempotencyStore)(nil)
	_ NonceStorer       = (*FileNonceStore)(nil)
)
