// Package api provides HTTP API handlers
package api

import (
	"time"
)

// TransferRequest represents the transfer submission request
type TransferRequest struct {
	FromID         string `json:"from_id" binding:"required"`
	ToID           string `json:"to_id" binding:"required"`
	AmountVND      int64  `json:"amount_vnd" binding:"required,min=1"`
	Memo           string `json:"memo"`
	InternalRef    string `json:"internal_ref" binding:"required"`
	IdempotencyKey string `json:"idempotency_key" binding:"required"`
	Nonce          string `json:"nonce" binding:"required"`
	Timestamp      int64  `json:"timestamp" binding:"required"`
	UserSignature  string `json:"user_sig,omitempty"` // Optional client signature
	TransferType   string `json:"transfer_type"`      // "intra_bank" or "inter_bank"
}

// TransferResponse represents the transfer submission response
type TransferResponse struct {
	Success     bool         `json:"success"`
	TxID        string       `json:"tx_id,omitempty"`
	Receipt     *ReceiptDTO  `json:"receipt,omitempty"`
	Error       *ErrorDetail `json:"error,omitempty"`
	ProcessedAt int64        `json:"processed_at"`
}

// ReceiptDTO is the API representation of a receipt
type ReceiptDTO struct {
	SchemaVersion       string                `json:"schema_version"`
	ChannelID           string                `json:"channel_id"`
	TxID                string                `json:"tx_id"`
	ChaincodeID         string                `json:"chaincode_id"`
	CommitmentHash      string                `json:"commitment_hash"`
	CommitmentOpening   *CommitmentOpeningDTO `json:"commitment_opening,omitempty"`
	CommitmentAlgorithm string                `json:"commitment_algorithm,omitempty"`
	BlockNumber         uint64                `json:"block_number"`
	BlockHash           string                `json:"block_hash,omitempty"`
	TxIndex             int                   `json:"tx_index,omitempty"`
	ValidationCode      int32                 `json:"validation_code"`
	ValidationCodeName  string                `json:"validation_code_name"`
	Endorsements        []EndorsementDTO      `json:"endorsements"`
	OriginMSPID         string                `json:"origin_msp_id,omitempty"`
	PolicyID            string                `json:"policy_id"`
	PolicyMet           bool                  `json:"policy_met"`
	Timestamps          TimestampsDTO         `json:"timestamps"`
	StatusEndpoint      string                `json:"status_endpoint,omitempty"`
	Observation         ObservationDTO        `json:"observation,omitempty"`
	InternalRef         string                `json:"internal_ref,omitempty"`
	ReceiptHash         string                `json:"receipt_hash"`
}

type CommitmentOpeningDTO struct {
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

type ObservationDTO struct {
	ObservedAt int64  `json:"observed_at"`
	AnchorType string `json:"anchor_type,omitempty"`
	AnchorRef  string `json:"anchor_ref,omitempty"`
}

// EndorsementDTO represents endorsement in API response
type EndorsementDTO struct {
	MSPID           string `json:"msp_id"`
	EndorserID      string `json:"endorser_id"`
	CertFingerprint string `json:"cert_fingerprint"`
	CertificatePEM  string `json:"certificate_pem,omitempty"`
	SignatureHex    string `json:"signature_hex"`
	Timestamp       int64  `json:"timestamp,omitempty"`
	SignatureValid  bool   `json:"signature_valid"`
	CertChainValid  bool   `json:"cert_chain_valid"`
}

// TimestampsDTO represents timestamps in API response
type TimestampsDTO struct {
	ClientSubmit    int64 `json:"client_submit"`
	EndorsementTime int64 `json:"endorsement_time"`
	OrdererReceive  int64 `json:"orderer_receive,omitempty"`
	BlockCommit     int64 `json:"block_commit"`
	ReceiptCreated  int64 `json:"receipt_created"`
}

// VerifyReceiptRequest represents receipt verification request
type VerifyReceiptRequest struct {
	Receipt                *ReceiptDTO `json:"receipt" binding:"required"`
	ExpectedCommitmentHash string      `json:"expected_commitment_hash,omitempty"`
	VerifyOnChain          bool        `json:"verify_on_chain"`
}

// VerifyReceiptResponse represents receipt verification response
type VerifyReceiptResponse struct {
	Valid                  bool                   `json:"valid"`
	Errors                 []string               `json:"errors,omitempty"`
	Warnings               []string               `json:"warnings,omitempty"`
	ReceiptHashValid       bool                   `json:"receipt_hash_valid"`
	CommitmentHashMatch    bool                   `json:"commitment_hash_match,omitempty"`
	CommitmentOpeningValid bool                   `json:"commitment_opening_valid,omitempty"`
	ValidationCodeValid    bool                   `json:"validation_code_valid"`
	PolicySatisfied        bool                   `json:"policy_satisfied"`
	EndorsementResults     []EndorsementVerifyDTO `json:"endorsement_results"`
	BlockVerified          bool                   `json:"block_verified,omitempty"`
	VerifiedAt             int64                  `json:"verified_at"`
}

// EndorsementVerifyDTO represents endorsement verification result
type EndorsementVerifyDTO struct {
	MSPID          string `json:"msp_id"`
	EndorserID     string `json:"endorser_id"`
	SignatureValid bool   `json:"signature_valid"`
	CertChainValid bool   `json:"cert_chain_valid"`
	CertExpired    bool   `json:"cert_expired"`
	CertRevoked    bool   `json:"cert_revoked"`
	Error          string `json:"error,omitempty"`
}

// ErrorDetail represents API error details
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// HealthResponse represents health check response
type HealthResponse struct {
	Status       string    `json:"status"`
	FabricStatus string    `json:"fabric_status"`
	Version      string    `json:"version"`
	Timestamp    time.Time `json:"timestamp"`
}

// Error codes
const (
	ErrCodeBadRequest          = "BAD_REQUEST"
	ErrCodeValidationFailed    = "VALIDATION_FAILED"
	ErrCodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	ErrCodeReplayDetected      = "REPLAY_DETECTED"
	ErrCodeFabricError         = "FABRIC_ERROR"
	ErrCodeInternalError       = "INTERNAL_ERROR"
	ErrCodeNotFound            = "NOT_FOUND"
	ErrCodeUnauthorized        = "UNAUTHORIZED"
)

// Transfer types
const (
	TransferTypeIntraBank = "intra_bank"
	TransferTypeInterBank = "inter_bank"
)
