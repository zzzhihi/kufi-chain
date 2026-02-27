package nodemgr

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/fabric-payment-gateway/internal/fabricops"
)

// Manager orchestrates join requests, voting, and channel config updates.
type Manager struct {
	Config *NodeConfig
	Store  *Store
	Gossip *Gossip

	// ApprovalCh receives a join request ID when majority is reached.
	// The CLI dashboard listens on this to display approval events.
	ApprovalCh chan string

	// NotifyCh receives approval notifications (for joining nodes waiting).
	NotifyCh chan *ApprovalNotification

	// RefreshCh signals the dashboard to re-render (non-blocking, buffered 1).
	RefreshCh chan struct{}
}

// NewManager creates a new node manager.
func NewManager(cfg *NodeConfig, store *Store) *Manager {
	gossip := NewGossip(store)
	return &Manager{
		Config:     cfg,
		Store:      store,
		Gossip:     gossip,
		ApprovalCh: make(chan string, 10),
		NotifyCh:   make(chan *ApprovalNotification, 10),
		RefreshCh:  make(chan struct{}, 1),
	}
}

// signalRefresh non-blocking send to RefreshCh.
func (m *Manager) signalRefresh() {
	select {
	case m.RefreshCh <- struct{}{}:
	default:
	}
}

// SubmitJoinRequest creates and stores a new join request, then gossips it.
func (m *Manager) SubmitJoinRequest(req *JoinRequest) error {
	if req.ID == "" {
		req.ID = generateID()
	}
	if req.Status == "" {
		req.Status = StatusPending
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now()
	}

	// Count existing peers to determine majority threshold
	peers, _ := m.Store.LoadPeers()
	req.TotalPeers = len(peers)

	if err := m.Store.SaveJoinRequest(req); err != nil {
		return fmt.Errorf("save join request: %w", err)
	}

	slog.Info("new join request stored",
		"id", req.ID, "org", req.OrgName,
		"msp", req.MSPID, "total_peers", req.TotalPeers,
		"required_votes", req.RequiredVotes())

	// Gossip to all known peers
	m.Gossip.BroadcastJoinRequest(req)
	m.signalRefresh()
	return nil
}

// ReceiveGossipedRequest handles a join request received via gossip from another peer.
func (m *Manager) ReceiveGossipedRequest(req *JoinRequest) error {
	// Check if we already have this request
	existing, _ := m.Store.LoadJoinRequest(req.ID)
	if existing != nil {
		// Merge votes from gossip that we don't have yet
		changed := false
		for _, v := range req.Votes {
			if !existing.HasVoted(v.VoterMSP) {
				existing.Votes = append(existing.Votes, v)
				changed = true
			}
		}
		if changed {
			m.Store.SaveJoinRequest(existing)
			m.checkMajority(existing)
		}
		return nil
	}

	// New request: store it
	if err := m.Store.SaveJoinRequest(req); err != nil {
		return err
	}

	slog.Info("received gossipped join request", "id", req.ID, "org", req.OrgName)
	m.signalRefresh()
	return nil
}

// CastVote records this node's vote on a join request and gossips it.
func (m *Manager) CastVote(requestID string, approve bool) error {
	req, err := m.Store.LoadJoinRequest(requestID)
	if err != nil {
		return fmt.Errorf("request not found: %w", err)
	}

	if req.Status != StatusPending {
		return fmt.Errorf("request %s is already %s", requestID, req.Status)
	}

	if req.HasVoted(m.Config.MSPID) {
		return fmt.Errorf("already voted on request %s", requestID)
	}

	vote := Vote{
		VoterOrg:  m.Config.OrgName,
		VoterMSP:  m.Config.MSPID,
		Approve:   approve,
		Timestamp: time.Now(),
	}
	req.Votes = append(req.Votes, vote)

	if err := m.Store.SaveJoinRequest(req); err != nil {
		return err
	}

	slog.Info("vote cast",
		"request", requestID, "approve", approve,
		"approvals", req.ApprovalCount(), "required", req.RequiredVotes())

	// Gossip the vote
	m.Gossip.BroadcastVote(requestID, &vote)
	m.signalRefresh()

	// Check if majority reached
	m.checkMajority(req)
	return nil
}

// ReceiveGossipedVote handles a vote received via gossip.
func (m *Manager) ReceiveGossipedVote(requestID string, vote *Vote) error {
	req, err := m.Store.LoadJoinRequest(requestID)
	if err != nil {
		slog.Debug("vote for unknown request", "id", requestID)
		return nil // ignore votes for unknown requests
	}

	if req.HasVoted(vote.VoterMSP) {
		return nil // already have this vote
	}

	req.Votes = append(req.Votes, *vote)
	m.Store.SaveJoinRequest(req)

	slog.Info("received gossipped vote",
		"request", requestID, "voter", vote.VoterOrg, "approve", vote.Approve)
	m.signalRefresh()

	m.checkMajority(req)
	return nil
}

// checkMajority checks if a request has enough approvals and triggers the approval flow.
func (m *Manager) checkMajority(req *JoinRequest) {
	if req.Status != StatusPending {
		return
	}
	if !req.MajorityReached() {
		return
	}

	slog.Info("MAJORITY REACHED for join request",
		"id", req.ID, "org", req.OrgName,
		"approvals", req.ApprovalCount(), "required", req.RequiredVotes())

	// Mark as approved
	now := time.Now()
	req.Status = StatusApproved
	req.ResolvedAt = &now
	m.Store.SaveJoinRequest(req)

	// Signal the CLI dashboard
	select {
	case m.ApprovalCh <- req.ID:
	default:
	}

	// If we're the bootstrap/orderer node, execute the channel config update
	if m.Config.Role == RoleBootstrap || m.Config.Role == RoleOrderer {
		go m.executeApproval(req)
	}
}

// executeApproval performs the channel config update to add the new org,
// collects signatures from approving peers, and notifies the joining node.
func (m *Manager) executeApproval(req *JoinRequest) {
	slog.Info("executing channel config update for approved org", "org", req.OrgName)
	fail := func(stage string, err error) {
		slog.Error("approval flow failed",
			"stage", stage,
			"org", req.OrgName,
			"request_id", req.ID,
			"err", err,
		)
		now := time.Now()
		req.Status = StatusRejected
		req.ResolvedAt = &now
		_ = m.Store.SaveJoinRequest(req)
		_ = m.Gossip.NotifyApproval(req.MgmtAddr, &ApprovalNotification{
			RequestID: req.ID,
			Approved:  false,
		})
	}

	useOrdererAdmin := m.Config.Role == RoleOrderer

	chOps := &fabricops.ChannelOps{
		DeployDir:      m.Config.DeployDir,
		ChannelName:    m.Config.ChannelName,
		OrdererAddr:    m.Config.OrdererAddr,
		NetworkDom:     m.Config.NetworkDomain,
		OrgName:        m.Config.OrgName,
		MSPID:          m.Config.MSPID,
		Domain:         m.Config.Domain,
		PeerPort:       m.Config.PeerPort,
		AdminPort:      m.Config.OrdererAdminPort,
		IsOrdererAdmin: useOrdererAdmin,
	}

	// First, unpack the new org's MSP to the local crypto directory
	if req.OrgMSPBundle != "" {
		targetDir := filepath.Join(m.Config.DeployDir, "crypto-config", "peerOrganizations")
		if err := fabricops.UnpackMSPBundle(req.OrgMSPBundle, targetDir); err != nil {
			fail("unpack_org_msp", err)
			return
		}
	}

	// Create config update envelope
	envelopePath, err := chOps.AddOrgToChannel(req.OrgDefinition)
	if err != nil {
		fail("create_config_update", err)
		return
	}

	// Sign with our own identity
	if err := chOps.SignConfigUpdate(envelopePath); err != nil {
		fail("sign_config_update", err)
		return
	}

	// Collect signatures from other approving peers
	envelopeData, _ := os.ReadFile(envelopePath)
	envelopeB64 := base64.StdEncoding.EncodeToString(envelopeData)

	peers, _ := m.Store.LoadPeers()
	for _, vote := range req.Votes {
		if !vote.Approve || vote.VoterMSP == m.Config.MSPID {
			continue // skip rejections and our own vote
		}
		// Find peer's management address
		for _, p := range peers {
			if p.MSPID == vote.VoterMSP {
				slog.Info("requesting signature from peer", "org", p.OrgName)

				signResp, err := m.Gossip.RequestSignature(p.MgmtAddr, &SignRequest{
					RequestID:  req.ID,
					EnvelopePB: envelopeB64,
				})
				if err != nil {
					slog.Error("failed to get signature", "org", p.OrgName, "err", err)
					continue
				}

				// Write the signed envelope back
				signed, _ := base64.StdEncoding.DecodeString(signResp.EnvelopePB)
				os.WriteFile(envelopePath, signed, 0o644)
				envelopeB64 = signResp.EnvelopePB
				break
			}
		}
	}

	// Submit the config update
	if err := chOps.SubmitConfigUpdate(envelopePath); err != nil {
		fail("submit_config_update", err)
		return
	}

	slog.Info("channel config updated — new org added!", "org", req.OrgName)

	// Register the new peer
	newPeer := PeerInfo{
		OrgName:  req.OrgName,
		MSPID:    req.MSPID,
		Domain:   req.Domain,
		PeerAddr: fmt.Sprintf("%s:%d", req.AnchorHost, req.AnchorPort),
		MgmtAddr: req.MgmtAddr,
		JoinedAt: time.Now(),
	}
	m.Store.AddPeer(newPeer)

	// Gossip the new peer info to all
	m.Gossip.BroadcastPeerJoined(&newPeer)

	// Notify the joining node
	ordererTLSCA, _ := chOps.ReadOrdererTLSCA()

	// For orderer nodes, always advertise external address if provided.
	ordererNotifAddr := m.Config.OrdererAddr
	if m.Config.Role == RoleOrderer && m.Config.ExternalHost != "" {
		port := m.Config.OrdererPort
		if port == 0 {
			port = 7050
		}
		ordererNotifAddr = fmt.Sprintf("%s:%d", m.Config.ExternalHost, port)
	}
	ordererHost := ordererNotifAddr
	if host, _, err := net.SplitHostPort(ordererNotifAddr); err == nil {
		ordererHost = host
	}
	ordererHostIP := resolveHostToIP(ordererHost)

	// Build orderer mgmt address for gossip routing
	ordererMgmtAddr := ""
	if m.Config.ExternalHost != "" && m.Config.MgmtPort > 0 {
		ordererMgmtAddr = fmt.Sprintf("http://%s:%d", m.Config.ExternalHost, m.Config.MgmtPort)
	}

	// Include all existing peers so the joining node can seed its peer list
	allPeers, _ := m.Store.LoadPeers()

	notif := &ApprovalNotification{
		RequestID:       req.ID,
		Approved:        true,
		OrdererAddr:     ordererNotifAddr,
		OrdererHostIP:   ordererHostIP,
		OrdererTLSCA:    ordererTLSCA,
		OrdererMgmtAddr: ordererMgmtAddr,
		ChannelName:     m.Config.ChannelName,
		NetworkDomain:   m.Config.NetworkDomain,
		ExistingPeers:   allPeers,
	}

	if err := m.Gossip.NotifyApproval(req.MgmtAddr, notif); err != nil {
		slog.Error("failed to notify joining node", "err", err)
	} else {
		slog.Info("joining node notified successfully", "org", req.OrgName)
	}
}

// HandleNotification processes an approval notification (for joining nodes).
func (m *Manager) HandleNotification(notif *ApprovalNotification) {
	select {
	case m.NotifyCh <- notif:
	default:
	}
}

// HandlePeerJoined processes a peer-joined gossip message.
func (m *Manager) HandlePeerJoined(peer *PeerInfo) {
	m.Store.AddPeer(*peer)
	slog.Info("new peer joined network", "org", peer.OrgName, "addr", peer.PeerAddr)
	m.signalRefresh()
}

// HandleHeartbeat updates the LastSeen timestamp for the heartbeat sender.
func (m *Manager) HandleHeartbeat(hb *Heartbeat) {
	m.Store.UpdatePeerLastSeen(hb.MSPID)
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func resolveHostToIP(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil {
			return ip.String()
		}
	}
	return ""
}
