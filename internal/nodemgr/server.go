package nodemgr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/fabric-payment-gateway/internal/fabricops"
)

const maxBodySize = 10 << 20 // 10 MB limit for request bodies

// Server runs the management HTTP API on each node.
type Server struct {
	manager *Manager
	port    int
	srv     *http.Server
}

// NewServer creates a management API server.
func NewServer(manager *Manager, port int) *Server {
	return &Server{
		manager: manager,
		port:    port,
	}
}

// Start runs the HTTP server (blocking). Call in a goroutine.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Join request endpoints
	mux.HandleFunc("/api/join-request", s.handleJoinRequest)
	mux.HandleFunc("/api/join-requests", s.handleListJoinRequests)
	mux.HandleFunc("/api/vote", s.handleVote)

	// Gossip endpoints
	mux.HandleFunc("/api/gossip/request", s.handleGossipRequest)
	mux.HandleFunc("/api/gossip/vote", s.handleGossipVote)
	mux.HandleFunc("/api/gossip/peer-joined", s.handleGossipPeerJoined)

	// Signing endpoint (for channel config updates)
	mux.HandleFunc("/api/sign-config-update", s.handleSignConfigUpdate)

	// Notification endpoint (for joining nodes)
	mux.HandleFunc("/api/notify/approved", s.handleNotifyApproved)

	// Status endpoints
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/peers", s.handlePeers)

	// Heartbeat endpoint
	mux.HandleFunc("/api/heartbeat", s.handleHeartbeat)

	// Chaincode lifecycle: trigger re-approval when a new org joins
	mux.HandleFunc("/api/chaincode/trigger-upgrade", s.handleChaincodeUpgrade)

	s.srv = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	slog.Info("management API starting", "port", s.port)
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// --- Handlers ---

func (s *Server) handleJoinRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, _ := readBody(r)
	var req JoinRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := s.manager.SubmitJoinRequest(&req); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]interface{}{
		"id":             req.ID,
		"status":         req.Status,
		"total_peers":    req.TotalPeers,
		"required_votes": req.RequiredVotes(),
	})
}

func (s *Server) handleListJoinRequests(w http.ResponseWriter, r *http.Request) {
	status := RequestStatus(r.URL.Query().Get("status"))
	requests, err := s.manager.Store.ListJoinRequests(status)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, requests)
}

func (s *Server) handleVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, _ := readBody(r)
	var payload struct {
		RequestID string `json:"request_id"`
		Approve   bool   `json:"approve"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := s.manager.CastVote(payload.RequestID, payload.Approve); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jsonOK(w, map[string]string{"status": "voted"})
}

func (s *Server) handleGossipRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, _ := readBody(r)
	var req JoinRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.manager.ReceiveGossipedRequest(&req)
	jsonOK(w, map[string]string{"status": "received"})
}

func (s *Server) handleGossipVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, _ := readBody(r)
	var payload struct {
		RequestID string `json:"request_id"`
		Vote      Vote   `json:"vote"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.manager.ReceiveGossipedVote(payload.RequestID, &payload.Vote)
	jsonOK(w, map[string]string{"status": "received"})
}

func (s *Server) handleGossipPeerJoined(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, _ := readBody(r)
	var peer PeerInfo
	if err := json.Unmarshal(body, &peer); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.manager.HandlePeerJoined(&peer)
	jsonOK(w, map[string]string{"status": "received"})
}

func (s *Server) handleSignConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, _ := readBody(r)
	var req SignRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	cfg := s.manager.Config
	chOps := &fabricops.ChannelOps{
		DeployDir:   cfg.DeployDir,
		ChannelName: cfg.ChannelName,
		OrdererAddr: cfg.OrdererAddr,
		NetworkDom:  cfg.NetworkDomain,
		OrgName:     cfg.OrgName,
		MSPID:       cfg.MSPID,
		Domain:      cfg.Domain,
		PeerPort:    cfg.PeerPort,
	}

	signed, err := chOps.SignEnvelopeBytes(req.EnvelopePB)
	if err != nil {
		jsonOK(w, SignResponse{Error: err.Error()})
		return
	}

	jsonOK(w, SignResponse{EnvelopePB: signed})
}

func (s *Server) handleNotifyApproved(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, _ := readBody(r)
	var notif ApprovalNotification
	if err := json.Unmarshal(body, &notif); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.manager.HandleNotification(&notif)
	jsonOK(w, map[string]string{"status": "received"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.manager.Config
	peers, _ := s.manager.Store.LoadPeers()
	pending, _ := s.manager.Store.ListJoinRequests(StatusPending)

	status := map[string]interface{}{
		"role":              cfg.Role,
		"org_name":          cfg.OrgName,
		"msp_id":            cfg.MSPID,
		"channel":           cfg.ChannelName,
		"network_domain":    cfg.NetworkDomain,
		"peer_port":         cfg.PeerPort,
		"mgmt_port":         cfg.MgmtPort,
		"orderer":           cfg.OrdererAddr,
		"orderer_mgmt_addr": cfg.OrdererMgmtAddr,
		"known_peers":       len(peers),
		"pending_requests":  len(pending),
	}
	jsonOK(w, status)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.manager.Store.LoadPeers()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, peers)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBody(r)
	if err != nil {
		jsonError(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var hb Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	s.manager.HandleHeartbeat(&hb)
	jsonOK(w, map[string]string{"status": "ok"})
}

func (s *Server) handleChaincodeUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Signal the main loop to re-evaluate the chaincode policy and trigger
	// re-approval + commit at the next sequence.
	s.manager.signalRefresh()
	slog.Info("chaincode upgrade triggered via API")
	jsonOK(w, map[string]string{"status": "triggered"})
}

// --- helpers ---

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxBodySize))
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
