// Package main implements the Governance chaincode for Hyperledger Fabric.
// It provides on-chain voting-based admission control for new organizations
// joining the network.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/hyperledger/fabric-chaincode-go/shim"
	"github.com/hyperledger/fabric-contract-api-go/contractapi"
)

// GovernanceContract provides functions for network governance.
type GovernanceContract struct {
	contractapi.Contract
}

// --- Data types ---

// JoinRequest represents an organization's request to join the network.
type JoinRequest struct {
	DocType     string            `json:"docType"`
	RequestID   string            `json:"requestId"`
	OrgName     string            `json:"orgName"`
	MSPID       string            `json:"mspId"`
	Domain      string            `json:"domain"`
	Description string            `json:"description,omitempty"`
	AnchorHost  string            `json:"anchorHost"`
	AnchorPort  int               `json:"anchorPort"`
	Status      string            `json:"status"`      // PENDING, APPROVED, REJECTED, EXPIRED
	Approvals   map[string]string `json:"approvals"`   // MSPID -> timestamp
	Rejections  map[string]string `json:"rejections"`  // MSPID -> timestamp
	RequiredAck int               `json:"requiredAck"` // number of approvals needed (majority)
	TotalOrgs   int               `json:"totalOrgs"`   // total existing orgs at request time
	CreatedAt   string            `json:"createdAt"`
	UpdatedAt   string            `json:"updatedAt"`
	ResolvedAt  string            `json:"resolvedAt,omitempty"`
	CreatorMSP  string            `json:"creatorMsp"`
}

// VoteRecord records an individual vote.
type VoteRecord struct {
	DocType   string `json:"docType"`
	RequestID string `json:"requestId"`
	VoterMSP  string `json:"voterMsp"`
	Decision  string `json:"decision"` // APPROVE or REJECT
	Reason    string `json:"reason,omitempty"`
	Timestamp string `json:"timestamp"`
}

// GovernancePolicy defines the voting policy for governance decisions.
type GovernancePolicy struct {
	DocType      string `json:"docType"`
	PolicyID     string `json:"policyId"`
	PolicyType   string `json:"policyType"`   // MAJORITY, UNANIMOUS, THRESHOLD
	ThresholdPct int    `json:"thresholdPct"` // percentage of approvals needed (e.g., 51)
	ExpiryHours  int    `json:"expiryHours"`  // hours before a request expires
	UpdatedAt    string `json:"updatedAt"`
	UpdatedByMSP string `json:"updatedByMsp"`
}

// Request statuses
const (
	StatusPending  = "PENDING"
	StatusApproved = "APPROVED"
	StatusRejected = "REJECTED"
	StatusExpired  = "EXPIRED"
)

const (
	defaultPolicyID   = "default-join-policy"
	defaultPolicyType = "MAJORITY"
	defaultThreshold  = 51
	defaultExpiryHrs  = 168 // 7 days
)

// --- Init ---

// InitLedger initialises the default governance policy.
func (g *GovernanceContract) InitLedger(ctx contractapi.TransactionContextInterface) error {
	existing, err := ctx.GetStub().GetState("policy:" + defaultPolicyID)
	if err != nil {
		return fmt.Errorf("check existing policy: %w", err)
	}
	if existing != nil {
		return nil // already initialised
	}

	policy := GovernancePolicy{
		DocType:      "governancePolicy",
		PolicyID:     defaultPolicyID,
		PolicyType:   defaultPolicyType,
		ThresholdPct: defaultThreshold,
		ExpiryHours:  defaultExpiryHrs,
		UpdatedAt:    txTimestamp(ctx),
		UpdatedByMSP: clientMSP(ctx),
	}
	data, _ := json.Marshal(policy)
	return ctx.GetStub().PutState("policy:"+defaultPolicyID, data)
}

// --- Join Request lifecycle ---

// RequestJoin creates a new join request. Should be invoked by the
// requesting org (or a sponsor org on their behalf).
func (g *GovernanceContract) RequestJoin(
	ctx contractapi.TransactionContextInterface,
	orgName, mspID, domain, anchorHost string,
	anchorPort int,
	description string,
) error {
	// Validate
	if orgName == "" || mspID == "" || domain == "" {
		return fmt.Errorf("orgName, mspID, and domain are required")
	}
	if anchorPort <= 0 || anchorPort > 65535 {
		return fmt.Errorf("invalid anchor port: %d", anchorPort)
	}

	requestID := ctx.GetStub().GetTxID()

	// Check if this MSP already has a pending request
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey("joinRequest", []string{})
	if err != nil {
		return fmt.Errorf("query existing requests: %w", err)
	}
	defer iter.Close()
	for iter.HasNext() {
		kv, _ := iter.Next()
		var existing JoinRequest
		if json.Unmarshal(kv.Value, &existing) == nil {
			if existing.MSPID == mspID && existing.Status == StatusPending {
				return fmt.Errorf("pending request already exists for %s: %s", mspID, existing.RequestID)
			}
		}
	}

	// Count existing orgs using the org registry
	totalOrgs := g.countExistingOrgs(ctx)
	requiredAck := (totalOrgs / 2) + 1 // majority
	if requiredAck < 1 {
		requiredAck = 1
	}

	now := txTimestamp(ctx)
	req := JoinRequest{
		DocType:     "joinRequest",
		RequestID:   requestID,
		OrgName:     orgName,
		MSPID:       mspID,
		Domain:      domain,
		Description: description,
		AnchorHost:  anchorHost,
		AnchorPort:  anchorPort,
		Status:      StatusPending,
		Approvals:   make(map[string]string),
		Rejections:  make(map[string]string),
		RequiredAck: requiredAck,
		TotalOrgs:   totalOrgs,
		CreatedAt:   now,
		UpdatedAt:   now,
		CreatorMSP:  clientMSP(ctx),
	}

	key, err := ctx.GetStub().CreateCompositeKey("joinRequest", []string{requestID})
	if err != nil {
		return fmt.Errorf("create composite key: %w", err)
	}

	data, _ := json.Marshal(req)
	if err := ctx.GetStub().PutState(key, data); err != nil {
		return fmt.Errorf("save join request: %w", err)
	}

	// Emit event
	ctx.GetStub().SetEvent("JoinRequested", data)
	return nil
}

// ApproveJoin casts an approval vote on a pending join request.
func (g *GovernanceContract) ApproveJoin(
	ctx contractapi.TransactionContextInterface,
	requestID string,
	reason string,
) error {
	return g.castVote(ctx, requestID, "APPROVE", reason)
}

// RejectJoin casts a rejection vote on a pending join request.
func (g *GovernanceContract) RejectJoin(
	ctx contractapi.TransactionContextInterface,
	requestID string,
	reason string,
) error {
	return g.castVote(ctx, requestID, "REJECT", reason)
}

func (g *GovernanceContract) castVote(
	ctx contractapi.TransactionContextInterface,
	requestID, decision, reason string,
) error {
	key, err := ctx.GetStub().CreateCompositeKey("joinRequest", []string{requestID})
	if err != nil {
		return fmt.Errorf("create key: %w", err)
	}

	data, err := ctx.GetStub().GetState(key)
	if err != nil || data == nil {
		return fmt.Errorf("join request not found: %s", requestID)
	}

	var req JoinRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("unmarshal request: %w", err)
	}

	if req.Status != StatusPending {
		return fmt.Errorf("request %s is no longer pending (status: %s)", requestID, req.Status)
	}

	// Check expiry using deterministic tx timestamp
	policy := g.getPolicy(ctx)
	txNow := txTime(ctx)
	if isExpired(req.CreatedAt, policy.ExpiryHours, txNow) {
		req.Status = StatusExpired
		req.UpdatedAt = txTimestamp(ctx)
		req.ResolvedAt = req.UpdatedAt
		updated, _ := json.Marshal(req)
		ctx.GetStub().PutState(key, updated)
		return fmt.Errorf("request %s has expired", requestID)
	}

	voterMSP := clientMSP(ctx)

	// Cannot vote on your own join request if you're the requesting org
	if voterMSP == req.MSPID {
		return fmt.Errorf("requesting org cannot vote on its own request")
	}

	// Check for duplicate vote
	if _, ok := req.Approvals[voterMSP]; ok {
		return fmt.Errorf("%s has already voted on this request", voterMSP)
	}
	if _, ok := req.Rejections[voterMSP]; ok {
		return fmt.Errorf("%s has already voted on this request", voterMSP)
	}

	now := txTimestamp(ctx)

	// Record vote
	vote := VoteRecord{
		DocType:   "voteRecord",
		RequestID: requestID,
		VoterMSP:  voterMSP,
		Decision:  decision,
		Reason:    reason,
		Timestamp: now,
	}
	voteKey, _ := ctx.GetStub().CreateCompositeKey("vote", []string{requestID, voterMSP})
	voteData, _ := json.Marshal(vote)
	ctx.GetStub().PutState(voteKey, voteData)

	// Apply vote
	if decision == "APPROVE" {
		req.Approvals[voterMSP] = now
	} else {
		req.Rejections[voterMSP] = now
	}
	req.UpdatedAt = now

	// Evaluate outcome
	if len(req.Approvals) >= req.RequiredAck {
		req.Status = StatusApproved
		req.ResolvedAt = now
		// Register org
		g.registerOrg(ctx, req.MSPID, req.OrgName, req.Domain)
	}
	// If enough rejections to make approval impossible
	maxPossibleApprovals := req.TotalOrgs - len(req.Rejections)
	if maxPossibleApprovals < req.RequiredAck {
		req.Status = StatusRejected
		req.ResolvedAt = now
	}

	updated, _ := json.Marshal(req)
	ctx.GetStub().PutState(key, updated)

	// Emit event
	eventName := "JoinVoteCast"
	if req.Status == StatusApproved {
		eventName = "JoinApproved"
	} else if req.Status == StatusRejected {
		eventName = "JoinRejected"
	}
	ctx.GetStub().SetEvent(eventName, updated)

	return nil
}

// --- Queries ---

// GetJoinRequest returns a specific join request.
func (g *GovernanceContract) GetJoinRequest(
	ctx contractapi.TransactionContextInterface,
	requestID string,
) (*JoinRequest, error) {
	key, err := ctx.GetStub().CreateCompositeKey("joinRequest", []string{requestID})
	if err != nil {
		return nil, err
	}
	data, err := ctx.GetStub().GetState(key)
	if err != nil || data == nil {
		return nil, fmt.Errorf("not found: %s", requestID)
	}
	var req JoinRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ListJoinRequests returns all join requests, optionally filtered by status.
func (g *GovernanceContract) ListJoinRequests(
	ctx contractapi.TransactionContextInterface,
	statusFilter string,
) ([]*JoinRequest, error) {
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey("joinRequest", []string{})
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer iter.Close()

	var results []*JoinRequest
	for iter.HasNext() {
		kv, _ := iter.Next()
		var req JoinRequest
		if json.Unmarshal(kv.Value, &req) == nil {
			if statusFilter == "" || req.Status == statusFilter {
				results = append(results, &req)
			}
		}
	}

	// Sort by creation time descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].CreatedAt > results[j].CreatedAt
	})

	return results, nil
}

// GetVotes returns all votes for a specific join request.
func (g *GovernanceContract) GetVotes(
	ctx contractapi.TransactionContextInterface,
	requestID string,
) ([]*VoteRecord, error) {
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey("vote", []string{requestID})
	if err != nil {
		return nil, fmt.Errorf("query votes: %w", err)
	}
	defer iter.Close()

	var results []*VoteRecord
	for iter.HasNext() {
		kv, _ := iter.Next()
		var vote VoteRecord
		if json.Unmarshal(kv.Value, &vote) == nil {
			results = append(results, &vote)
		}
	}
	return results, nil
}

// --- Policy management ---

// UpdatePolicy updates the governance policy (admin-only).
func (g *GovernanceContract) UpdatePolicy(
	ctx contractapi.TransactionContextInterface,
	policyType string,
	thresholdPct int,
	expiryHours int,
) error {
	if policyType != "MAJORITY" && policyType != "UNANIMOUS" && policyType != "THRESHOLD" {
		return fmt.Errorf("invalid policy type: %s (allowed: MAJORITY, UNANIMOUS, THRESHOLD)", policyType)
	}
	if thresholdPct < 1 || thresholdPct > 100 {
		return fmt.Errorf("threshold must be 1-100")
	}
	if expiryHours < 1 {
		return fmt.Errorf("expiry must be at least 1 hour")
	}

	policy := GovernancePolicy{
		DocType:      "governancePolicy",
		PolicyID:     defaultPolicyID,
		PolicyType:   policyType,
		ThresholdPct: thresholdPct,
		ExpiryHours:  expiryHours,
		UpdatedAt:    txTimestamp(ctx),
		UpdatedByMSP: clientMSP(ctx),
	}
	data, _ := json.Marshal(policy)
	return ctx.GetStub().PutState("policy:"+defaultPolicyID, data)
}

// GetPolicy returns the current governance policy.
func (g *GovernanceContract) GetPolicy(
	ctx contractapi.TransactionContextInterface,
) (*GovernancePolicy, error) {
	p := g.getPolicy(ctx)
	return &p, nil
}

// --- Org registry ---

// RegisterExistingOrgs initialises the org registry with the founding orgs.
// Should be called once during network bootstrap.
func (g *GovernanceContract) RegisterExistingOrgs(
	ctx contractapi.TransactionContextInterface,
	orgsJSON string,
) error {
	type OrgEntry struct {
		MSPID  string `json:"mspId"`
		Name   string `json:"name"`
		Domain string `json:"domain"`
	}
	var orgs []OrgEntry
	if err := json.Unmarshal([]byte(orgsJSON), &orgs); err != nil {
		return fmt.Errorf("parse orgs JSON: %w", err)
	}
	for _, org := range orgs {
		g.registerOrg(ctx, org.MSPID, org.Name, org.Domain)
	}
	return nil
}

// ListOrgs returns all registered organizations.
func (g *GovernanceContract) ListOrgs(
	ctx contractapi.TransactionContextInterface,
) ([]string, error) {
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey("orgRegistry", []string{})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var orgs []string
	for iter.HasNext() {
		kv, _ := iter.Next()
		orgs = append(orgs, string(kv.Value))
	}
	return orgs, nil
}

// --- internal helpers ---

func (g *GovernanceContract) registerOrg(ctx contractapi.TransactionContextInterface, mspID, name, domain string) {
	key, _ := ctx.GetStub().CreateCompositeKey("orgRegistry", []string{mspID})
	entry := map[string]string{
		"mspId": mspID, "name": name, "domain": domain,
		"registeredAt": txTimestamp(ctx),
	}
	data, _ := json.Marshal(entry)
	ctx.GetStub().PutState(key, data)
}

func (g *GovernanceContract) countExistingOrgs(ctx contractapi.TransactionContextInterface) int {
	iter, err := ctx.GetStub().GetStateByPartialCompositeKey("orgRegistry", []string{})
	if err != nil {
		return 0
	}
	defer iter.Close()
	count := 0
	for iter.HasNext() {
		iter.Next()
		count++
	}
	return count
}

func (g *GovernanceContract) getPolicy(ctx contractapi.TransactionContextInterface) GovernancePolicy {
	data, err := ctx.GetStub().GetState("policy:" + defaultPolicyID)
	if err != nil || data == nil {
		return GovernancePolicy{
			PolicyType:   defaultPolicyType,
			ThresholdPct: defaultThreshold,
			ExpiryHours:  defaultExpiryHrs,
		}
	}
	var p GovernancePolicy
	json.Unmarshal(data, &p)
	return p
}

func clientMSP(ctx contractapi.TransactionContextInterface) string {
	msp, _ := ctx.GetClientIdentity().GetMSPID()
	return msp
}

func txTimestamp(ctx contractapi.TransactionContextInterface) string {
	ts, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return time.Unix(ts.Seconds, int64(ts.Nanos)).UTC().Format(time.RFC3339)
}

func txTime(ctx contractapi.TransactionContextInterface) time.Time {
	ts, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return time.Now().UTC()
	}
	return time.Unix(ts.Seconds, int64(ts.Nanos)).UTC()
}

func isExpired(createdAt string, expiryHours int, now time.Time) bool {
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return false
	}
	return now.After(created.Add(time.Duration(expiryHours) * time.Hour))
}

// --- main ---

func main() {
	chaincode, err := contractapi.NewChaincode(&GovernanceContract{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating governance chaincode: %v\n", err)
		os.Exit(1)
	}

	// Support CCAAS (Chaincode-as-a-Service) mode
	ccAddr := os.Getenv("CHAINCODE_SERVER_ADDRESS")
	ccID := os.Getenv("CHAINCODE_ID")

	if ccAddr != "" && ccID != "" {
		server := &shim.ChaincodeServer{
			CCID:    ccID,
			Address: ccAddr,
			CC:      chaincode,
			TLSProps: shim.TLSProperties{
				Disabled: true,
			},
		}
		fmt.Printf("Governance chaincode starting in CCAAS mode on %s\n", ccAddr)
		if err := server.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting governance chaincode server: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Println("Governance chaincode starting in peer mode")
		if err := shim.Start(chaincode); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting governance chaincode: %v\n", err)
			os.Exit(1)
		}
	}
}
