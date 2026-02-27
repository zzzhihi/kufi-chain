// Package receipt provides verifiable receipt generation and verification
package receipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// SchemaVersion defines the current receipt schema version
const SchemaVersion = "v1"

// Receipt represents a verifiable transaction receipt
type Receipt struct {
	// Schema version for forward compatibility
	SchemaVersion string `json:"schema_version"`

	// Basic transaction identifiers
	ChannelID    string `json:"channel_id"`
	TxID         string `json:"tx_id"`
	ChaincodeID  string `json:"chaincode_id"`

	// Commitment hash linking to actual transaction data
	CommitmentHash string `json:"commitment_hash"`

	// Block information
	BlockNumber uint64 `json:"block_number"`
	BlockHash   string `json:"block_hash,omitempty"`
	TxIndex     int    `json:"tx_index,omitempty"`

	// Validation status
	ValidationCode     int32  `json:"validation_code"`
	ValidationCodeName string `json:"validation_code_name"`

	// Endorsement information
	Endorsements []EndorsementRecord `json:"endorsements"`
	PolicyID     string              `json:"policy_id"`
	PolicyMet    bool                `json:"policy_met"`

	// Timestamps
	Timestamps ReceiptTimestamps `json:"timestamps"`

	// Verification aids
	StatusEndpoint string `json:"status_endpoint,omitempty"`
	
	// Internal reference from client
	InternalRef string `json:"internal_ref,omitempty"`

	// Receipt signature (gateway signs the receipt)
	ReceiptSignature string `json:"receipt_signature,omitempty"`
	ReceiptHash      string `json:"receipt_hash"`
}

// EndorsementRecord represents an endorser's information in the receipt
type EndorsementRecord struct {
	MSPID            string `json:"msp_id"`
	EndorserID       string `json:"endorser_id"`
	CertFingerprint  string `json:"cert_fingerprint"`
	CertificatePEM   string `json:"certificate_pem,omitempty"` // Optional full cert
	SignatureHex     string `json:"signature_hex"`
	Timestamp        int64  `json:"timestamp,omitempty"`
	SignatureValid   bool   `json:"signature_valid"`
	CertChainValid   bool   `json:"cert_chain_valid"`
}

// ReceiptTimestamps contains various timestamps related to the transaction
type ReceiptTimestamps struct {
	ClientSubmit    int64 `json:"client_submit"`     // When client submitted request
	EndorsementTime int64 `json:"endorsement_time"`  // When endorsements collected
	OrdererReceive  int64 `json:"orderer_receive,omitempty"` // When orderer received
	BlockCommit     int64 `json:"block_commit"`      // When block committed
	ReceiptCreated  int64 `json:"receipt_created"`   // When receipt generated
}

// TransferCommitment represents the data used to generate commitment hash
type TransferCommitment struct {
	ChannelID   string `json:"channel_id"`
	ChaincodeID string `json:"chaincode_id"`
	Function    string `json:"function"`
	FromID      string `json:"from_id"`
	ToID        string `json:"to_id"`
	AmountVND   int64  `json:"amount_vnd"`
	InternalRef string `json:"internal_ref"`
	Timestamp   int64  `json:"timestamp"`
	Nonce       string `json:"nonce"`
}

// ComputeCommitmentHash computes the commitment hash for a transfer
func ComputeCommitmentHash(commitment *TransferCommitment) string {
	data, err := json.Marshal(commitment)
	if err != nil {
		// Should never happen with a well-defined struct, but don't silently proceed
		panic(fmt.Sprintf("failed to marshal transfer commitment: %v", err))
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// ComputeReceiptHash computes the hash of receipt content (excluding signature)
func (r *Receipt) ComputeReceiptHash() string {
	// Create a copy without signature for hashing
	hashData := struct {
		SchemaVersion  string              `json:"schema_version"`
		ChannelID      string              `json:"channel_id"`
		TxID           string              `json:"tx_id"`
		CommitmentHash string              `json:"commitment_hash"`
		BlockNumber    uint64              `json:"block_number"`
		BlockHash      string              `json:"block_hash"`
		ValidationCode int32               `json:"validation_code"`
		Endorsements   []EndorsementRecord `json:"endorsements"`
		PolicyID       string              `json:"policy_id"`
		PolicyMet      bool                `json:"policy_met"`
		Timestamps     ReceiptTimestamps   `json:"timestamps"`
	}{
		SchemaVersion:  r.SchemaVersion,
		ChannelID:      r.ChannelID,
		TxID:           r.TxID,
		CommitmentHash: r.CommitmentHash,
		BlockNumber:    r.BlockNumber,
		BlockHash:      r.BlockHash,
		ValidationCode: r.ValidationCode,
		Endorsements:   r.Endorsements,
		PolicyID:       r.PolicyID,
		PolicyMet:      r.PolicyMet,
		Timestamps:     r.Timestamps,
	}

	data, _ := json.Marshal(hashData)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// ValidationCodeToName converts validation code to human-readable name
func ValidationCodeToName(code int32) string {
	// Fabric validation codes
	names := map[int32]string{
		0:   "VALID",
		1:   "NIL_ENVELOPE",
		2:   "BAD_PAYLOAD",
		3:   "BAD_COMMON_HEADER",
		4:   "BAD_CREATOR_SIGNATURE",
		5:   "INVALID_ENDORSER_TRANSACTION",
		6:   "INVALID_CONFIG_TRANSACTION",
		7:   "UNSUPPORTED_TX_PAYLOAD",
		8:   "BAD_PROPOSAL_TXID",
		9:   "DUPLICATE_TXID",
		10:  "ENDORSEMENT_POLICY_FAILURE",
		11:  "MVCC_READ_CONFLICT",
		12:  "PHANTOM_READ_CONFLICT",
		13:  "UNKNOWN_TX_TYPE",
		14:  "TARGET_CHAIN_NOT_FOUND",
		15:  "MARSHAL_TX_ERROR",
		16:  "NIL_TXACTION",
		17:  "EXPIRED_CHAINCODE",
		18:  "CHAINCODE_VERSION_CONFLICT",
		19:  "BAD_HEADER_EXTENSION",
		20:  "BAD_CHANNEL_HEADER",
		21:  "BAD_RESPONSE_PAYLOAD",
		22:  "BAD_RWSET",
		23:  "ILLEGAL_WRITESET",
		24:  "INVALID_WRITESET",
		254: "NOT_VALIDATED",
		255: "INVALID_OTHER_REASON",
	}

	if name, ok := names[code]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN_%d", code)
}

// ReceiptBuilder builds verifiable receipts
type ReceiptBuilder struct {
	channelID      string
	chaincodeID    string
	statusEndpoint string
	includeFullCerts bool
}

// NewReceiptBuilder creates a new receipt builder
func NewReceiptBuilder(channelID, chaincodeID, statusEndpoint string, includeFullCerts bool) *ReceiptBuilder {
	return &ReceiptBuilder{
		channelID:      channelID,
		chaincodeID:    chaincodeID,
		statusEndpoint: statusEndpoint,
		includeFullCerts: includeFullCerts,
	}
}

// BuildInput contains all data needed to build a receipt
type BuildInput struct {
	TxID           string
	CommitmentHash string
	BlockNumber    uint64
	BlockHash      string
	TxIndex        int
	ValidationCode int32
	Endorsements   []EndorsementInput
	PolicyID       string
	PolicyMet      bool
	InternalRef    string
	ClientSubmitTime time.Time
	EndorsementTime  time.Time
	CommitTime       time.Time
}

// EndorsementInput contains endorsement data for receipt building
type EndorsementInput struct {
	MSPID          string
	EndorserID     string
	CertPEM        []byte
	Signature      []byte
	Timestamp      time.Time
	SignatureValid bool
	CertChainValid bool
}

// Build creates a verifiable receipt from input data
func (rb *ReceiptBuilder) Build(input *BuildInput) (*Receipt, error) {
	receipt := &Receipt{
		SchemaVersion:      SchemaVersion,
		ChannelID:          rb.channelID,
		TxID:               input.TxID,
		ChaincodeID:        rb.chaincodeID,
		CommitmentHash:     input.CommitmentHash,
		BlockNumber:        input.BlockNumber,
		BlockHash:          input.BlockHash,
		TxIndex:            input.TxIndex,
		ValidationCode:     input.ValidationCode,
		ValidationCodeName: ValidationCodeToName(input.ValidationCode),
		PolicyID:           input.PolicyID,
		PolicyMet:          input.PolicyMet,
		StatusEndpoint:     rb.statusEndpoint,
		InternalRef:        input.InternalRef,
		Timestamps: ReceiptTimestamps{
			ClientSubmit:    input.ClientSubmitTime.UnixMilli(),
			EndorsementTime: input.EndorsementTime.UnixMilli(),
			BlockCommit:     input.CommitTime.UnixMilli(),
			ReceiptCreated:  time.Now().UnixMilli(),
		},
	}

	// Build endorsement records
	for _, e := range input.Endorsements {
		record := EndorsementRecord{
			MSPID:          e.MSPID,
			EndorserID:     e.EndorserID,
			SignatureHex:   hex.EncodeToString(e.Signature),
			SignatureValid: e.SignatureValid,
			CertChainValid: e.CertChainValid,
		}

		// Compute certificate fingerprint from DER bytes (matches verifier)
		if len(e.CertPEM) > 0 {
			block, _ := pem.Decode(e.CertPEM)
			if block != nil {
				hash := sha256.Sum256(block.Bytes)
				record.CertFingerprint = hex.EncodeToString(hash[:])
			} else {
				// Fallback: hash raw bytes if PEM decode fails
				hash := sha256.Sum256(e.CertPEM)
				record.CertFingerprint = hex.EncodeToString(hash[:])
			}

			if rb.includeFullCerts {
				record.CertificatePEM = string(e.CertPEM)
			}
		}

		if !e.Timestamp.IsZero() {
			record.Timestamp = e.Timestamp.UnixMilli()
		}

		receipt.Endorsements = append(receipt.Endorsements, record)
	}

	// Compute receipt hash
	receipt.ReceiptHash = receipt.ComputeReceiptHash()

	return receipt, nil
}

// ToJSON serializes receipt to JSON
func (r *Receipt) ToJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// FromJSON deserializes receipt from JSON
func FromJSON(data []byte) (*Receipt, error) {
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, fmt.Errorf("failed to unmarshal receipt: %w", err)
	}
	return &receipt, nil
}
