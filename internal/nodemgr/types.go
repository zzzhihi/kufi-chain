// Package nodemgr provides decentralized node management for the Fabric network.
// Each node runs a management HTTP server that handles join requests, voting,
// and peer gossip to coordinate network membership changes.
package nodemgr

import (
	"encoding/base64"
	"time"
)

// NodeRole represents the role of this node in the network.
type NodeRole string

const (
	RoleBootstrap NodeRole = "bootstrap" // (legacy) runs orderer + first peer
	RoleOrderer   NodeRole = "orderer"   // dedicated orderer node — no peer/gateway
	RolePeer      NodeRole = "peer"      // peer node — runs peer + CouchDB + gateway
)

const OrdererMSPID = "OrdererMSP"

// RequestStatus represents the status of a join request.
type RequestStatus string

const (
	StatusPending  RequestStatus = "pending"
	StatusApproved RequestStatus = "approved"
	StatusRejected RequestStatus = "rejected"
)

// NodeConfig holds this node's configuration (persisted to .kufichain/node.json).
type NodeConfig struct {
	Role             NodeRole `json:"role"`
	OrgName          string   `json:"org_name"`
	MSPID            string   `json:"msp_id"`
	Domain           string   `json:"domain"`
	NetworkDomain    string   `json:"network_domain"`
	ChannelName      string   `json:"channel_name"`
	PeerPort         int      `json:"peer_port"`
	ChaincodePort    int      `json:"chaincode_port"`
	CouchDBPort      int      `json:"couchdb_port"`
	OpsPort          int      `json:"operations_port"`
	GatewayPort      int      `json:"gateway_port,omitempty"` // payment gateway HTTP port (default 8080)
	MgmtPort         int      `json:"mgmt_port"`
	OrdererPort      int      `json:"orderer_port,omitempty"`       // orderer listen port (default 7050)
	OrdererAdminPort int      `json:"orderer_admin_port,omitempty"` // osnadmin port (default 7053)
	OrdererAddr      string   `json:"orderer_addr"`                 // host:port of orderer
	OrdererMgmtAddr  string   `json:"orderer_mgmt_addr,omitempty"`  // orderer mgmt API for gossip routing
	ExternalHost     string   `json:"external_host"`                // this node's public IP/hostname
	DataDir          string   `json:"data_dir"`
	DeployDir        string   `json:"deploy_dir"`
}

// PeerInfo describes a known peer in the network.
type PeerInfo struct {
	OrgName  string    `json:"org_name"`
	MSPID    string    `json:"msp_id"`
	Domain   string    `json:"domain"`
	PeerAddr string    `json:"peer_addr"` // fabric peer address (host:port)
	MgmtAddr string    `json:"mgmt_addr"` // management API address (http://host:port)
	JoinedAt time.Time `json:"joined_at"`
	LastSeen time.Time `json:"last_seen,omitempty"` // updated by heartbeat
}

// JoinRequest represents a request from a new org to join the network.
type JoinRequest struct {
	ID            string        `json:"id"`
	OrgName       string        `json:"org_name"`
	MSPID         string        `json:"msp_id"`
	Domain        string        `json:"domain"`
	PeerHost      string        `json:"peer_host,omitempty"` // host/IP peers should use to reach this node's peer
	AnchorHost    string        `json:"anchor_host"`
	AnchorPort    int           `json:"anchor_port"`
	PeerPort      int           `json:"peer_port"`
	MgmtAddr      string        `json:"mgmt_addr"`      // new node's management address
	OrgDefinition string        `json:"org_definition"` // JSON from configtxgen -printOrg
	OrgMSPBundle  string        `json:"org_msp_bundle"` // base64 tar.gz of org MSP dir
	Status        RequestStatus `json:"status"`
	Votes         []Vote        `json:"votes"`
	TotalPeers    int           `json:"total_peers"` // number of existing peers at request time
	CreatedAt     time.Time     `json:"created_at"`
	ResolvedAt    *time.Time    `json:"resolved_at,omitempty"`
}

// Vote represents a single org's vote on a join request.
type Vote struct {
	VoterOrg  string    `json:"voter_org"`
	VoterMSP  string    `json:"voter_msp"`
	Approve   bool      `json:"approve"`
	Timestamp time.Time `json:"timestamp"`
}

// ApprovalNotification is sent to the joining node when approved.
type ApprovalNotification struct {
	RequestID       string      `json:"request_id"`
	Approved        bool        `json:"approved"`
	OrdererAddr     string      `json:"orderer_addr"`
	OrdererHostIP   string      `json:"orderer_host_ip,omitempty"` // resolved IP for orderer.<networkDomain> mapping
	OrdererTLSCA    string      `json:"orderer_tls_ca"`            // base64-encoded PEM
	OrdererMgmtAddr string      `json:"orderer_mgmt_addr"`         // orderer mgmt API (http://host:port)
	ChannelName     string      `json:"channel_name"`
	NetworkDomain   string      `json:"network_domain"`
	ExistingPeers   []PeerInfo  `json:"existing_peers,omitempty"`   // seed peer list for joining node
	ExistingBundles []OrgBundle `json:"existing_bundles,omitempty"` // org crypto bundles so the joining node can talk TLS to existing peers
}

// OrgBundle carries a peer org's crypto material to another node.
type OrgBundle struct {
	OrgName string `json:"org_name"`
	MSPID   string `json:"msp_id"`
	Domain  string `json:"domain"`
	Bundle  string `json:"bundle"`
}

// Heartbeat is sent periodically by each node to its gossip targets.
type Heartbeat struct {
	MSPID    string `json:"msp_id"`
	OrgName  string `json:"org_name"`
	MgmtAddr string `json:"mgmt_addr"`
}

// SignRequest asks a peer to sign a config update envelope.
type SignRequest struct {
	RequestID  string `json:"request_id"`
	EnvelopePB string `json:"envelope_pb"` // base64-encoded protobuf
}

// SignResponse returns the signed envelope.
type SignResponse struct {
	EnvelopePB string `json:"envelope_pb"` // base64-encoded signed protobuf
	Error      string `json:"error,omitempty"`
}

// RequiredVotes returns the number of approvals needed (majority).
func (r *JoinRequest) RequiredVotes() int {
	if r.TotalPeers <= 0 {
		return 1
	}
	return r.TotalPeers/2 + 1
}

// ApprovalCount returns the current number of approvals.
func (r *JoinRequest) ApprovalCount() int {
	n := 0
	for _, v := range r.Votes {
		if !v.Approve {
			continue
		}
		// Bootstrap case: when no peer org exists yet, allow the orderer vote
		// to admit the first peer into the network.
		if r.TotalPeers == 0 || v.VoterMSP != OrdererMSPID {
			n++
		}
	}
	return n
}

// MajorityReached checks if the join request has enough approvals.
func (r *JoinRequest) MajorityReached() bool {
	return r.ApprovalCount() >= r.RequiredVotes()
}

// HasVoted checks if an org has already voted on this request.
func (r *JoinRequest) HasVoted(mspID string) bool {
	for _, v := range r.Votes {
		if v.VoterMSP == mspID {
			return true
		}
	}
	return false
}

// DecodeOrgMSP decodes the base64 tar.gz bundle.
func (r *JoinRequest) DecodeOrgMSP() ([]byte, error) {
	return base64.StdEncoding.DecodeString(r.OrgMSPBundle)
}
