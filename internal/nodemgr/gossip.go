package nodemgr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Gossip handles peer-to-peer message forwarding to all known peers and the orderer.
type Gossip struct {
	store  *Store
	client *http.Client
}

// NewGossip creates a new gossip handler.
func NewGossip(store *Store) *Gossip {
	return &Gossip{
		store: store,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     60 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}
}

// gossipTargets returns all management addresses that should receive gossip
// messages: all known peers + the orderer (if we know its mgmt address and
// it isn't ourselves).
func (g *Gossip) gossipTargets() []string {
	peers, _ := g.store.LoadPeers()
	addrs := make([]string, 0, len(peers)+1)
	for _, p := range peers {
		addrs = append(addrs, p.MgmtAddr)
	}

	// Also include the orderer mgmt address if stored (peer nodes learn
	// this from the approval notification).
	cfg, err := g.store.LoadNodeConfig()
	if err == nil && cfg.OrdererMgmtAddr != "" {
		// Don't duplicate if orderer is already in the peers list, and
		// don't send to ourselves.
		dup := false
		for _, a := range addrs {
			if a == cfg.OrdererMgmtAddr {
				dup = true
				break
			}
		}
		if !dup {
			addrs = append(addrs, cfg.OrdererMgmtAddr)
		}
	}
	return addrs
}

// BroadcastJoinRequest forwards a join request to all known peers and the orderer.
func (g *Gossip) BroadcastJoinRequest(req *JoinRequest) {
	targets := g.gossipTargets()
	if len(targets) == 0 {
		return
	}

	data, _ := json.Marshal(req)
	for _, addr := range targets {
		go g.sendToPeer(addr, "/api/gossip/request", data)
	}
}

// BroadcastVote forwards a vote to all known peers and the orderer.
func (g *Gossip) BroadcastVote(requestID string, vote *Vote) {
	targets := g.gossipTargets()
	if len(targets) == 0 {
		return
	}

	msg := map[string]interface{}{
		"request_id": requestID,
		"vote":       vote,
	}
	data, _ := json.Marshal(msg)
	for _, addr := range targets {
		go g.sendToPeer(addr, "/api/gossip/vote", data)
	}
}

// BroadcastPeerJoined informs all peers and the orderer about a newly accepted peer.
func (g *Gossip) BroadcastPeerJoined(peer *PeerInfo) {
	targets := g.gossipTargets()
	if len(targets) == 0 {
		return
	}

	data, _ := json.Marshal(peer)
	for _, addr := range targets {
		go g.sendToPeer(addr, "/api/gossip/peer-joined", data)
	}
}

// SendHeartbeat sends a heartbeat to all gossip targets.
func (g *Gossip) SendHeartbeat(hb *Heartbeat) {
	targets := g.gossipTargets()
	if len(targets) == 0 {
		return
	}
	data, _ := json.Marshal(hb)
	for _, addr := range targets {
		go g.sendToPeer(addr, "/api/heartbeat", data)
	}
}

func (g *Gossip) sendToPeer(mgmtAddr, path string, body []byte) {
	url := mgmtAddr + path
	resp, err := g.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Debug("gossip send failed", "url", url, "err", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain for connection reuse

	if resp.StatusCode != http.StatusOK {
		slog.Debug("gossip peer returned non-200", "url", url, "status", resp.StatusCode)
	}
}

// NotifyApproval sends an approval notification to a joining node.
func (g *Gossip) NotifyApproval(mgmtAddr string, notif *ApprovalNotification) error {
	data, _ := json.Marshal(notif)
	url := mgmtAddr + "/api/notify/approved"

	resp, err := g.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("notify %s: %w", mgmtAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("notify %s: HTTP %d — %s", mgmtAddr, resp.StatusCode, string(body))
	}
	return nil
}

// RequestSignature asks a remote peer to sign a config update envelope.
func (g *Gossip) RequestSignature(mgmtAddr string, req *SignRequest) (*SignResponse, error) {
	data, _ := json.Marshal(req)
	url := mgmtAddr + "/api/sign-config-update"

	resp, err := g.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("sign request to %s: %w", mgmtAddr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var signResp SignResponse
	if err := json.Unmarshal(body, &signResp); err != nil {
		return nil, fmt.Errorf("parse sign response: %w", err)
	}
	if signResp.Error != "" {
		return nil, fmt.Errorf("remote sign error: %s", signResp.Error)
	}
	return &signResp, nil
}
