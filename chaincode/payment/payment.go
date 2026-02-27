// Package main implements the Payment chaincode for Hyperledger Fabric
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/hyperledger/fabric-chaincode-go/shim"
	"github.com/hyperledger/fabric-contract-api-go/contractapi"
)

// PaymentContract provides functions for managing payment transfers
type PaymentContract struct {
	contractapi.Contract
}

// TransferIntent represents a transfer intent stored on-chain
// Only metadata and commitment hash are stored on public ledger
type TransferIntent struct {
	DocType        string `json:"docType"`
	TxID           string `json:"txId"`
	CommitmentHash string `json:"commitmentHash"`
	InternalRef    string `json:"internalRef"`
	PolicyID       string `json:"policyId"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
	SettledAt      int64  `json:"settledAt,omitempty"`
	CreatorMSP     string `json:"creatorMsp"`
}

// TransferDetails represents detailed transfer data stored in PDC
type TransferDetails struct {
	DocType     string `json:"docType"`
	TxID        string `json:"txId"`
	FromID      string `json:"fromId"`
	ToID        string `json:"toId"`
	AmountVND   int64  `json:"amountVnd"`
	Memo        string `json:"memo,omitempty"`
	InternalRef string `json:"internalRef"`
	Timestamp   int64  `json:"timestamp"`
	SenderMSP   string `json:"senderMsp"`
	ReceiverMSP string `json:"receiverMsp,omitempty"`
}

// Transfer status constants
const (
	StatusPending   = "PENDING"
	StatusSettled   = "SETTLED"
	StatusCancelled = "CANCELLED"
	StatusFailed    = "FAILED"
)

// Collection name prefixes (resolved dynamically with MSP ID)
const (
	CollectionIntraBankPrefix = "collectionIntraBank_"
	CollectionInterBank       = "collectionInterBank"
	CollectionRegulator       = "collectionRegulator"
	CollectionHighRisk        = "collectionHighRisk"
)

// resolveIntraBankCollection returns the per-org intra-bank collection name
func resolveIntraBankCollection(mspID string) string {
	// Strip trailing "MSP" suffix to get org name for collection
	// e.g. "VPBankMSP" -> "collectionIntraBank_VPBank"
	orgName := mspID
	if len(orgName) > 3 && orgName[len(orgName)-3:] == "MSP" {
		orgName = orgName[:len(orgName)-3]
	}
	return CollectionIntraBankPrefix + orgName
}

// Allowed transfer statuses for query validation
var allowedStatuses = map[string]bool{
	StatusPending:   true,
	StatusSettled:   true,
	StatusCancelled: true,
	StatusFailed:    true,
}

// CreateTransferIntent creates a new transfer intent
// Public state stores only commitment hash and metadata
// Private data collection stores the actual transfer details
func (pc *PaymentContract) CreateTransferIntent(
	ctx contractapi.TransactionContextInterface,
	commitmentHash string,
	internalRef string,
	policyID string,
) error {
	// Get transaction ID
	txID := ctx.GetStub().GetTxID()

	// Get creator MSP
	creatorMSP, err := pc.getCreatorMSP(ctx)
	if err != nil {
		return fmt.Errorf("failed to get creator MSP: %w", err)
	}

	// Check if transfer already exists (idempotency)
	existing, err := ctx.GetStub().GetState(txID)
	if err != nil {
		return fmt.Errorf("failed to check existing state: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("transfer intent already exists: %s", txID)
	}

	// Create transfer intent (public state) - use deterministic tx timestamp
	txTs, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return fmt.Errorf("failed to get tx timestamp: %w", err)
	}
	now := txTs.Seconds*1000 + int64(txTs.Nanos/1000000)
	intent := TransferIntent{
		DocType:        "transferIntent",
		TxID:           txID,
		CommitmentHash: commitmentHash,
		InternalRef:    internalRef,
		PolicyID:       policyID,
		Status:         StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
		CreatorMSP:     creatorMSP,
	}

	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("failed to marshal transfer intent: %w", err)
	}

	// Store public state
	if err := ctx.GetStub().PutState(txID, intentJSON); err != nil {
		return fmt.Errorf("failed to put state: %w", err)
	}

	// Process private data from transient map
	if err := pc.processPrivateData(ctx, txID, creatorMSP, policyID); err != nil {
		return fmt.Errorf("failed to process private data: %w", err)
	}

	// Emit event
	eventPayload := map[string]string{
		"txId":           txID,
		"commitmentHash": commitmentHash,
		"status":         StatusPending,
	}
	eventJSON, _ := json.Marshal(eventPayload)
	if err := ctx.GetStub().SetEvent("TransferIntentCreated", eventJSON); err != nil {
		return fmt.Errorf("failed to set event: %w", err)
	}

	return nil
}

// processPrivateData stores transfer details in appropriate PDC
func (pc *PaymentContract) processPrivateData(
	ctx contractapi.TransactionContextInterface,
	txID string,
	creatorMSP string,
	policyID string,
) error {
	// Get transient data
	transientMap, err := ctx.GetStub().GetTransient()
	if err != nil {
		return fmt.Errorf("failed to get transient map: %w", err)
	}

	// Determine which collection to use based on policy
	var collectionName string
	switch policyID {
	case "inter_bank_standard":
		collectionName = CollectionInterBank
	case "high_risk":
		collectionName = CollectionHighRisk
	default:
		collectionName = resolveIntraBankCollection(creatorMSP)
	}

	// Check if private data exists for this collection
	privateDataJSON, ok := transientMap[collectionName]
	if !ok {
		// Try default collection names
		for name, data := range transientMap {
			if len(data) > 0 {
				privateDataJSON = data
				collectionName = name
				break
			}
		}
	}

	if len(privateDataJSON) == 0 {
		// No private data provided, skip
		return nil
	}

	// Parse and validate private data
	var inputData map[string]interface{}
	if err := json.Unmarshal(privateDataJSON, &inputData); err != nil {
		return fmt.Errorf("failed to unmarshal private data: %w", err)
	}

	// Create transfer details
	details := TransferDetails{
		DocType:     "transferDetails",
		TxID:        txID,
		SenderMSP:   creatorMSP,
		InternalRef: getString(inputData, "internal_ref"),
		Timestamp:   getInt64(inputData, "timestamp"),
		FromID:      getString(inputData, "from_id"),
		ToID:        getString(inputData, "to_id"),
		AmountVND:   getInt64(inputData, "amount_vnd"),
		Memo:        getString(inputData, "memo"),
	}

	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("failed to marshal transfer details: %w", err)
	}

	// Store in private data collection
	if err := ctx.GetStub().PutPrivateData(collectionName, txID, detailsJSON); err != nil {
		return fmt.Errorf("failed to put private data: %w", err)
	}

	return nil
}

// SettleTransfer marks a transfer as settled
func (pc *PaymentContract) SettleTransfer(
	ctx contractapi.TransactionContextInterface,
	txID string,
) error {
	// Get existing transfer intent
	intentJSON, err := ctx.GetStub().GetState(txID)
	if err != nil {
		return fmt.Errorf("failed to get state: %w", err)
	}
	if intentJSON == nil {
		return fmt.Errorf("transfer intent not found: %s", txID)
	}

	var intent TransferIntent
	if err := json.Unmarshal(intentJSON, &intent); err != nil {
		return fmt.Errorf("failed to unmarshal transfer intent: %w", err)
	}

	// Verify status
	if intent.Status != StatusPending {
		return fmt.Errorf("transfer cannot be settled, current status: %s", intent.Status)
	}

	// Verify caller is the creator of this transfer
	creatorMSP, err := pc.getCreatorMSP(ctx)
	if err != nil {
		return fmt.Errorf("failed to get creator MSP: %w", err)
	}
	if creatorMSP != intent.CreatorMSP {
		return fmt.Errorf("only creator org can settle transfer")
	}

	// Update status - use deterministic tx timestamp
	txTs, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return fmt.Errorf("failed to get tx timestamp: %w", err)
	}
	now := txTs.Seconds*1000 + int64(txTs.Nanos/1000000)
	intent.Status = StatusSettled
	intent.UpdatedAt = now
	intent.SettledAt = now

	updatedJSON, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("failed to marshal updated intent: %w", err)
	}

	if err := ctx.GetStub().PutState(txID, updatedJSON); err != nil {
		return fmt.Errorf("failed to put state: %w", err)
	}

	// Emit event
	eventPayload := map[string]string{
		"txId":   txID,
		"status": StatusSettled,
	}
	eventJSON, _ := json.Marshal(eventPayload)
	ctx.GetStub().SetEvent("TransferSettled", eventJSON)

	return nil
}

// CancelTransfer cancels a pending transfer
func (pc *PaymentContract) CancelTransfer(
	ctx contractapi.TransactionContextInterface,
	txID string,
	reason string,
) error {
	// Get existing transfer intent
	intentJSON, err := ctx.GetStub().GetState(txID)
	if err != nil {
		return fmt.Errorf("failed to get state: %w", err)
	}
	if intentJSON == nil {
		return fmt.Errorf("transfer intent not found: %s", txID)
	}

	var intent TransferIntent
	if err := json.Unmarshal(intentJSON, &intent); err != nil {
		return fmt.Errorf("failed to unmarshal transfer intent: %w", err)
	}

	// Verify status
	if intent.Status != StatusPending {
		return fmt.Errorf("transfer cannot be cancelled, current status: %s", intent.Status)
	}

	// Verify caller is creator
	creatorMSP, err := pc.getCreatorMSP(ctx)
	if err != nil {
		return fmt.Errorf("failed to get creator MSP: %w", err)
	}
	if creatorMSP != intent.CreatorMSP {
		return fmt.Errorf("only creator can cancel transfer")
	}

	// Update status - use deterministic tx timestamp
	txTs, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return fmt.Errorf("failed to get tx timestamp: %w", err)
	}
	now := txTs.Seconds*1000 + int64(txTs.Nanos/1000000)
	intent.Status = StatusCancelled
	intent.UpdatedAt = now

	updatedJSON, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("failed to marshal updated intent: %w", err)
	}

	if err := ctx.GetStub().PutState(txID, updatedJSON); err != nil {
		return fmt.Errorf("failed to put state: %w", err)
	}

	// Emit event
	eventPayload := map[string]interface{}{
		"txId":   txID,
		"status": StatusCancelled,
		"reason": reason,
	}
	eventJSON, _ := json.Marshal(eventPayload)
	ctx.GetStub().SetEvent("TransferCancelled", eventJSON)

	return nil
}

// QueryTransfer retrieves a transfer intent by ID
func (pc *PaymentContract) QueryTransfer(
	ctx contractapi.TransactionContextInterface,
	txID string,
) (string, error) {
	intentJSON, err := ctx.GetStub().GetState(txID)
	if err != nil {
		return "", fmt.Errorf("failed to get state: %w", err)
	}
	if intentJSON == nil {
		return "", fmt.Errorf("transfer intent not found: %s", txID)
	}

	return string(intentJSON), nil
}

// getTransferIntent is an internal helper that returns the struct
func (pc *PaymentContract) getTransferIntent(
	ctx contractapi.TransactionContextInterface,
	txID string,
) (*TransferIntent, error) {
	intentJSON, err := ctx.GetStub().GetState(txID)
	if err != nil {
		return nil, fmt.Errorf("failed to get state: %w", err)
	}
	if intentJSON == nil {
		return nil, fmt.Errorf("transfer intent not found: %s", txID)
	}

	var intent TransferIntent
	if err := json.Unmarshal(intentJSON, &intent); err != nil {
		return nil, fmt.Errorf("failed to unmarshal transfer intent: %w", err)
	}

	return &intent, nil
}

// QueryTransferDetails retrieves transfer details from PDC
// Only members of the collection can access this
func (pc *PaymentContract) QueryTransferDetails(
	ctx contractapi.TransactionContextInterface,
	collectionName string,
	txID string,
) (string, error) {
	detailsJSON, err := ctx.GetStub().GetPrivateData(collectionName, txID)
	if err != nil {
		return "", fmt.Errorf("failed to get private data: %w", err)
	}
	if detailsJSON == nil {
		return "", fmt.Errorf("transfer details not found: %s", txID)
	}

	return string(detailsJSON), nil
}

// QueryTransfersByStatus queries transfers by status using rich queries
// Note: This requires CouchDB as state database
func (pc *PaymentContract) QueryTransfersByStatus(
	ctx contractapi.TransactionContextInterface,
	status string,
) (string, error) {
	// Validate status against allowlist to prevent CouchDB injection
	if !allowedStatuses[status] {
		return "", fmt.Errorf("invalid status: %s, must be one of PENDING, SETTLED, CANCELLED, FAILED", status)
	}
	queryString := fmt.Sprintf(`{"selector":{"docType":"transferIntent","status":"%s"}}`, status)

	resultsIterator, err := ctx.GetStub().GetQueryResult(queryString)
	if err != nil {
		return "", fmt.Errorf("failed to execute query: %w", err)
	}
	defer resultsIterator.Close()

	var results []json.RawMessage
	for resultsIterator.HasNext() {
		queryResponse, err := resultsIterator.Next()
		if err != nil {
			return "", fmt.Errorf("failed to get next result: %w", err)
		}
		results = append(results, queryResponse.Value)
	}

	if results == nil {
		return "[]", nil
	}
	out, _ := json.Marshal(results)
	return string(out), nil
}

// QueryTransfersByInternalRef queries transfers by internal reference
func (pc *PaymentContract) QueryTransfersByInternalRef(
	ctx contractapi.TransactionContextInterface,
	internalRef string,
) (string, error) {
	// Sanitize internalRef to prevent CouchDB query injection
	internalRef = sanitizeQueryInput(internalRef)
	queryString := fmt.Sprintf(`{"selector":{"docType":"transferIntent","internalRef":"%s"}}`, internalRef)

	resultsIterator, err := ctx.GetStub().GetQueryResult(queryString)
	if err != nil {
		return "", fmt.Errorf("failed to execute query: %w", err)
	}
	defer resultsIterator.Close()

	var results []json.RawMessage
	for resultsIterator.HasNext() {
		queryResponse, err := resultsIterator.Next()
		if err != nil {
			return "", fmt.Errorf("failed to get next result: %w", err)
		}
		results = append(results, queryResponse.Value)
	}

	if results == nil {
		return "[]", nil
	}
	out, _ := json.Marshal(results)
	return string(out), nil
}

// VerifyCommitmentHash verifies that a commitment hash matches the stored value
func (pc *PaymentContract) VerifyCommitmentHash(
	ctx contractapi.TransactionContextInterface,
	txID string,
	commitmentHash string,
) (bool, error) {
	intent, err := pc.getTransferIntent(ctx, txID)
	if err != nil {
		return false, err
	}

	return intent.CommitmentHash == commitmentHash, nil
}

// GetHistory returns the history of a transfer intent
func (pc *PaymentContract) GetHistory(
	ctx contractapi.TransactionContextInterface,
	txID string,
) (string, error) {
	historyIterator, err := ctx.GetStub().GetHistoryForKey(txID)
	if err != nil {
		return "", fmt.Errorf("failed to get history: %w", err)
	}
	defer historyIterator.Close()

	var history []map[string]interface{}
	for historyIterator.HasNext() {
		modification, err := historyIterator.Next()
		if err != nil {
			return "", fmt.Errorf("failed to get next history entry: %w", err)
		}

		var intent TransferIntent
		json.Unmarshal(modification.Value, &intent)

		entry := map[string]interface{}{
			"txId":      modification.TxId,
			"timestamp": modification.Timestamp.AsTime().UnixMilli(),
			"isDelete":  modification.IsDelete,
			"value":     intent,
		}
		history = append(history, entry)
	}

	out, _ := json.Marshal(history)
	return string(out), nil
}

// Helper functions

func (pc *PaymentContract) getCreatorMSP(ctx contractapi.TransactionContextInterface) (string, error) {
	creator, err := ctx.GetClientIdentity().GetMSPID()
	if err != nil {
		return "", err
	}
	return creator, nil
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// sanitizeQueryInput removes characters that could break CouchDB JSON selectors
func sanitizeQueryInput(s string) string {
	// Remove any characters that could escape the JSON string context
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		// Allow alphanumeric, dash, underscore, dot
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			result = append(result, c)
		}
	}
	return string(result)
}

func getInt64(m map[string]interface{}, key string) int64 {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case float64:
			return int64(val)
		case int64:
			return val
		case int:
			return int64(val)
		}
	}
	return 0
}

func main() {
	chaincode, err := contractapi.NewChaincode(&PaymentContract{})
	if err != nil {
		fmt.Printf("Error creating payment chaincode: %v\n", err)
		return
	}

	// Check if running as external chaincode service (ccaas mode)
	ccid := os.Getenv("CHAINCODE_ID")
	ccAddr := os.Getenv("CHAINCODE_SERVER_ADDRESS")
	if ccid != "" && ccAddr != "" {
		// Run as chaincode server (ccaas)
		server := &shim.ChaincodeServer{
			CCID:    ccid,
			Address: ccAddr,
			CC:      chaincode,
			TLSProps: shim.TLSProperties{
				Disabled: true,
			},
		}
		fmt.Printf("Starting chaincode server at %s with CCID %s\n", ccAddr, ccid)
		if err := server.Start(); err != nil {
			fmt.Printf("Error starting chaincode server: %v\n", err)
		}
	} else {
		// Run as embedded chaincode (traditional mode)
		if err := chaincode.Start(); err != nil {
			fmt.Printf("Error starting payment chaincode: %v\n", err)
		}
	}
}
