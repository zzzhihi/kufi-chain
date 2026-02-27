package nodemgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store provides file-based persistence for node state.
// All data lives under the .kufichain/ directory.
type Store struct {
	baseDir string
	mu      sync.RWMutex
}

// NewStore creates a store rooted at the given directory.
func NewStore(baseDir string) *Store {
	return &Store{baseDir: baseDir}
}

// Init creates the data directory structure.
func (s *Store) Init() error {
	dirs := []string{
		s.baseDir,
		filepath.Join(s.baseDir, "requests"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return nil
}

// --- NodeConfig ---

func (s *Store) nodeConfigPath() string {
	return filepath.Join(s.baseDir, "node.json")
}

// SaveNodeConfig persists the node configuration.
func (s *Store) SaveNodeConfig(cfg *NodeConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.nodeConfigPath(), cfg)
}

// LoadNodeConfig reads the node configuration.
func (s *Store) LoadNodeConfig() (*NodeConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cfg NodeConfig
	if err := readJSON(s.nodeConfigPath(), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// NodeConfigExists checks if node.json exists.
func (s *Store) NodeConfigExists() bool {
	_, err := os.Stat(s.nodeConfigPath())
	return err == nil
}

// --- Peers ---

func (s *Store) peersPath() string {
	return filepath.Join(s.baseDir, "peers.json")
}

// SavePeers persists the known peers list.
func (s *Store) SavePeers(peers []PeerInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.peersPath(), peers)
}

// LoadPeers reads the known peers list.
func (s *Store) LoadPeers() ([]PeerInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var peers []PeerInfo
	if err := readJSON(s.peersPath(), &peers); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return peers, nil
}

// AddPeer adds a peer if not already present (by MSPID).
func (s *Store) AddPeer(peer PeerInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var peers []PeerInfo
	_ = readJSON(s.peersPath(), &peers) // OK if not exists

	for i, p := range peers {
		if p.MSPID == peer.MSPID {
			peers[i] = peer // update existing
			return writeJSON(s.peersPath(), peers)
		}
	}
	peers = append(peers, peer)
	return writeJSON(s.peersPath(), peers)
}

// UpdatePeerLastSeen atomically updates LastSeen for a peer identified by MSPID.
func (s *Store) UpdatePeerLastSeen(mspID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var peers []PeerInfo
	if err := readJSON(s.peersPath(), &peers); err != nil {
		return
	}
	for i, p := range peers {
		if p.MSPID == mspID {
			peers[i].LastSeen = time.Now()
			writeJSON(s.peersPath(), peers)
			return
		}
	}
}

// --- JoinRequests ---

func (s *Store) requestPath(id string) string {
	return filepath.Join(s.baseDir, "requests", id+".json")
}

// SaveJoinRequest persists a join request.
func (s *Store) SaveJoinRequest(req *JoinRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.requestPath(req.ID), req)
}

// LoadJoinRequest reads a single join request.
func (s *Store) LoadJoinRequest(id string) (*JoinRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var req JoinRequest
	if err := readJSON(s.requestPath(id), &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ListJoinRequests returns all join requests, optionally filtered by status.
func (s *Store) ListJoinRequests(status RequestStatus) ([]*JoinRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	dir := filepath.Join(s.baseDir, "requests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []*JoinRequest
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var req JoinRequest
		if err := readJSON(filepath.Join(dir, e.Name()), &req); err != nil {
			continue
		}
		if status == "" || req.Status == status {
			result = append(result, &req)
		}
	}
	return result, nil
}

// --- helpers ---

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readJSON(path string, v interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
