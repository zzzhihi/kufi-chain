// Package api provides HTTP API handlers for the payment gateway
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/fabric-payment-gateway/internal/config"
	"github.com/fabric-payment-gateway/internal/fabric"
	"github.com/fabric-payment-gateway/internal/receipt"
)

// Handler contains all HTTP handlers
type Handler struct {
	config           *config.Config
	fabricClient     *fabric.Client
	blockQuery       *fabric.BlockQuery
	receiptBuilder   *receipt.ReceiptBuilder
	receiptVerifier  *receipt.Verifier
	receiptStore     ReceiptStorer
	idempotencyStore IdempotencyStorer
	nonceStore       NonceStorer
	logger           *zap.Logger
}

// NewHandler creates a new Handler instance
func NewHandler(
	cfg *config.Config,
	fabricClient *fabric.Client,
	logger *zap.Logger,
) (*Handler, error) {
	// Create persistent stores
	receiptStore, err := NewFileReceiptStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create receipt store: %w", err)
	}

	idempotencyStore, err := NewFileIdempotencyStore(cfg.Transaction.IdempotencyTTL, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create idempotency store: %w", err)
	}

	nonceStore, err := NewFileNonceStore(cfg.Transaction.NonceWindow, cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create nonce store: %w", err)
	}

	return &Handler{
		config:       cfg,
		fabricClient: fabricClient,
		blockQuery:   fabric.NewBlockQuery(fabricClient, logger),
		receiptBuilder: receipt.NewReceiptBuilder(
			cfg.Fabric.ChannelName,
			cfg.Fabric.Chaincode.Name,
			cfg.Receipt.StatusEndpoint,
			cfg.Receipt.IncludeFullCerts,
		),
		receiptVerifier:  receipt.NewVerifier(),
		receiptStore:     receiptStore,
		idempotencyStore: idempotencyStore,
		nonceStore:       nonceStore,
		logger:           logger,
	}, nil
}

// RegisterRoutes registers all API routes
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	v1 := r.Group("/v1")
	{
		v1.POST("/transfer", h.SubmitTransfer)
		v1.GET("/receipt/:tx_id", h.GetReceipt)
		v1.POST("/receipt/verify", h.VerifyReceipt)
	}

	// Health check
	r.GET("/health", h.HealthCheck)
}

// SubmitTransfer handles POST /v1/transfer
func (h *Handler) SubmitTransfer(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.config.Fabric.Timeouts.Submit+h.config.Fabric.Timeouts.CommitStatus)
	defer cancel()

	var req TransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.respondError(c, http.StatusBadRequest, ErrCodeBadRequest, "Invalid request body", err.Error())
		return
	}

	// Validate request
	if err := h.validateTransferRequest(&req); err != nil {
		h.respondError(c, http.StatusBadRequest, ErrCodeValidationFailed, "Validation failed", err.Error())
		return
	}

	// Check idempotency
	if existingReceipt := h.idempotencyStore.Get(req.IdempotencyKey); existingReceipt != nil {
		h.logger.Info("Returning cached result for idempotency key",
			zap.String("idempotencyKey", req.IdempotencyKey))
		c.JSON(http.StatusOK, TransferResponse{
			Success:     true,
			TxID:        existingReceipt.TxID,
			Receipt:     h.receiptToDTO(existingReceipt),
			ProcessedAt: time.Now().UnixMilli(),
		})
		return
	}

	// Check for replay (nonce + timestamp)
	if h.nonceStore.Exists(req.Nonce) {
		h.respondError(c, http.StatusConflict, ErrCodeReplayDetected, "Replay detected", "Nonce already used")
		return
	}

	// Validate timestamp within window
	now := time.Now().UnixMilli()
	timeDiff := now - req.Timestamp
	if timeDiff < 0 || timeDiff > h.config.Transaction.NonceWindow.Milliseconds() {
		h.respondError(c, http.StatusBadRequest, ErrCodeReplayDetected, "Request expired or future timestamp", "")
		return
	}

	// Record nonce
	h.nonceStore.Add(req.Nonce, time.Now())

	// Determine transfer type and policy
	transferType := req.TransferType
	if transferType == "" {
		transferType = TransferTypeIntraBank
	}

	policyID := h.determinePolicyID(&req)

	h.logger.Info("Processing transfer request",
		zap.String("internalRef", req.InternalRef),
		zap.String("transferType", transferType),
		zap.String("policyID", policyID),
	)

	// Compute commitment hash
	commitment := &receipt.TransferCommitment{
		ChannelID:   h.config.Fabric.ChannelName,
		ChaincodeID: h.config.Fabric.Chaincode.Name,
		Function:    "CreateTransferIntent",
		FromID:      req.FromID,
		ToID:        req.ToID,
		AmountVND:   req.AmountVND,
		InternalRef: req.InternalRef,
		Timestamp:   req.Timestamp,
		Nonce:       req.Nonce,
	}
	commitmentHash := receipt.ComputeCommitmentHash(commitment)

	// Prepare private data
	privateData := map[string]interface{}{
		"from_id":      req.FromID,
		"to_id":        req.ToID,
		"amount_vnd":   req.AmountVND,
		"memo":         req.Memo,
		"internal_ref": req.InternalRef,
		"timestamp":    req.Timestamp,
	}
	privateDataJSON, _ := json.Marshal(privateData)

	// Determine collection based on transfer type
	// For intra-bank, use org-specific collection: collectionIntraBank_<OrgName>
	var collectionName string
	if req.AmountVND >= h.config.Transaction.HighRiskThreshold {
		collectionName = "collectionHighRisk"
	} else if transferType == TransferTypeInterBank {
		collectionName = h.config.Collections.InterBank
	} else {
		// Compute org-specific intra-bank collection from MSP ID
		orgName := strings.TrimSuffix(h.config.Fabric.MSPID, "MSP")
		collectionName = h.config.Collections.IntraBank + "_" + orgName
	}

	// Prepare transient data for PDC
	transientData := map[string][]byte{
		collectionName: privateDataJSON,
	}

	// Determine endorsing orgs — for per-org PDC we must include the owner org
	var endorsingOrgs []string
	if transferType == TransferTypeIntraBank {
		// Single-org collection: endorse only on the owning org's peer
		endorsingOrgs = []string{h.config.Fabric.MSPID}
	}

	// Submit to Fabric
	clientSubmitTime := time.Now()
	result, err := h.fabricClient.SubmitTransactionWithPDC(
		ctx,
		"CreateTransferIntent",
		transientData,
		endorsingOrgs,
		commitmentHash,
		req.InternalRef,
		policyID,
	)
	if err != nil && isMissingCollectionError(err) {
		h.logger.Warn("Collection is missing on current chaincode definition, retrying submit without private data",
			zap.String("internalRef", req.InternalRef),
			zap.String("collection", collectionName),
			zap.Error(err),
		)
		// Retry without private data but keep explicit endorsing orgs to bypass service discovery
		result, err = h.fabricClient.SubmitTransactionWithPDC(
			ctx,
			"CreateTransferIntent",
			nil, // no private data
			endorsingOrgs,
			commitmentHash,
			req.InternalRef,
			policyID,
		)
	}
	if err != nil {
		h.logger.Error("Failed to submit transaction",
			zap.String("internalRef", req.InternalRef),
			zap.Error(err),
		)
		h.respondError(c, http.StatusInternalServerError, ErrCodeFabricError, "Failed to submit transaction", err.Error())
		return
	}

	// Query real block hash from ledger
	var blockHash string
	blockInfo, err := h.blockQuery.GetBlockByTxID(ctx, result.TxID)
	if err != nil {
		h.logger.Warn("Failed to query block hash, using computed fallback",
			zap.String("txID", result.TxID),
			zap.Error(err),
		)
		// Fallback: compute deterministic hash from available data
		blockHashData := fmt.Sprintf("block:%d:tx:%s:commit:%s", result.BlockNumber, result.TxID, commitmentHash)
		blockHashBytes := sha256.Sum256([]byte(blockHashData))
		blockHash = hex.EncodeToString(blockHashBytes[:])
	} else {
		blockHash = blockInfo.BlockHash
	}

	txInfo, err := h.blockQuery.GetTransactionByID(ctx, result.TxID)
	if err != nil {
		h.logger.Warn("Failed to query committed transaction details for receipt",
			zap.String("txID", result.TxID),
			zap.Error(err),
		)
	}

	// Build receipt
	buildInput := &receipt.BuildInput{
		TxID:             result.TxID,
		CommitmentHash:   commitmentHash,
		BlockNumber:      result.BlockNumber,
		BlockHash:        blockHash,
		ValidationCode:   result.ValidationCode,
		PolicyID:         policyID,
		PolicyMet:        result.ValidationCode == 0,
		InternalRef:      req.InternalRef,
		ClientSubmitTime: clientSubmitTime,
		EndorsementTime:  result.CommitTimestamp,
		CommitTime:       result.CommitTimestamp,
	}

	if txInfo != nil {
		buildInput.TxIndex = txInfo.TxIndex
		h.appendCommittedEndorsements(buildInput, txInfo.Endorsements)
	}
	if len(buildInput.Endorsements) == 0 {
		h.appendSubmittedEndorsements(buildInput, result.Endorsements)
	}
	if len(buildInput.Endorsements) == 0 {
		h.logger.Warn("Receipt built without endorsements",
			zap.String("txID", result.TxID),
			zap.Int("submittedEndorsements", len(result.Endorsements)),
		)
	}

	txReceipt, err := h.receiptBuilder.Build(buildInput)
	if err != nil {
		h.logger.Error("Failed to build receipt", zap.Error(err))
		h.respondError(c, http.StatusInternalServerError, ErrCodeInternalError, "Failed to build receipt", err.Error())
		return
	}

	// Store receipt
	h.receiptStore.Store(result.TxID, txReceipt)
	h.idempotencyStore.Store(req.IdempotencyKey, txReceipt)

	h.logger.Info("Transfer completed successfully",
		zap.String("txID", result.TxID),
		zap.Uint64("blockNumber", result.BlockNumber),
		zap.String("internalRef", req.InternalRef),
	)

	c.JSON(http.StatusOK, TransferResponse{
		Success:     true,
		TxID:        result.TxID,
		Receipt:     h.receiptToDTO(txReceipt),
		ProcessedAt: time.Now().UnixMilli(),
	})
}

func isMissingCollectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "collection") && strings.Contains(msg, "could not be found")
}

// GetReceipt handles GET /v1/receipt/:tx_id
func (h *Handler) GetReceipt(c *gin.Context) {
	txID := c.Param("tx_id")
	if txID == "" {
		h.respondError(c, http.StatusBadRequest, ErrCodeBadRequest, "Transaction ID required", "")
		return
	}

	rcpt := h.receiptStore.Get(txID)
	if rcpt == nil {
		h.respondError(c, http.StatusNotFound, ErrCodeNotFound, "Receipt not found", "")
		return
	}

	if len(rcpt.Endorsements) == 0 {
		rebuilt, err := h.rebuildReceiptFromLedger(c.Request.Context(), rcpt)
		if err != nil {
			h.logger.Warn("Failed to rebuild receipt endorsements from ledger",
				zap.String("txID", txID),
				zap.Error(err),
			)
		} else if rebuilt != nil {
			rcpt = rebuilt
			h.receiptStore.Store(txID, rcpt)
		}
	}

	c.JSON(http.StatusOK, h.receiptToDTO(rcpt))
}

// VerifyReceipt handles POST /v1/receipt/verify
func (h *Handler) VerifyReceipt(c *gin.Context) {
	var req VerifyReceiptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.respondError(c, http.StatusBadRequest, ErrCodeBadRequest, "Invalid request body", err.Error())
		return
	}

	// Convert DTO to receipt
	rcpt := h.dtoToReceipt(req.Receipt)

	// Perform verification
	result, err := h.receiptVerifier.VerifyReceipt(rcpt, req.ExpectedCommitmentHash)
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, ErrCodeInternalError, "Verification failed", err.Error())
		return
	}

	// Convert to response
	response := VerifyReceiptResponse{
		Valid:               result.Valid,
		Errors:              result.Errors,
		Warnings:            result.Warnings,
		ReceiptHashValid:    result.ReceiptHashValid,
		CommitmentHashMatch: result.CommitmentHashMatch,
		ValidationCodeValid: result.ValidationCodeValid,
		PolicySatisfied:     result.PolicySatisfied,
		BlockVerified:       result.BlockVerified,
		VerifiedAt:          result.VerifiedAt,
	}

	for _, er := range result.EndorsementResults {
		response.EndorsementResults = append(response.EndorsementResults, EndorsementVerifyDTO{
			MSPID:          er.MSPID,
			EndorserID:     er.EndorserID,
			SignatureValid: er.SignatureValid,
			CertChainValid: er.CertChainValid,
			CertExpired:    er.CertExpired,
			CertRevoked:    er.CertRevoked,
			Error:          er.Error,
		})
	}

	c.JSON(http.StatusOK, response)
}

// HealthCheck handles GET /health
func (h *Handler) HealthCheck(c *gin.Context) {
	fabricStatus := "connected"
	if !h.fabricClient.IsConnected() {
		fabricStatus = "disconnected"
	}

	c.JSON(http.StatusOK, HealthResponse{
		Status:       "ok",
		FabricStatus: fabricStatus,
		Version:      "1.0.0",
		Timestamp:    time.Now(),
	})
}

// validateTransferRequest validates the transfer request
func (h *Handler) validateTransferRequest(req *TransferRequest) error {
	if req.AmountVND <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	if req.FromID == req.ToID {
		return fmt.Errorf("sender and receiver cannot be the same")
	}
	if len(req.InternalRef) == 0 {
		return fmt.Errorf("internal_ref is required")
	}
	if len(req.IdempotencyKey) == 0 {
		return fmt.Errorf("idempotency_key is required")
	}
	if len(req.Nonce) < 16 {
		return fmt.Errorf("nonce must be at least 16 characters")
	}
	return nil
}

// determinePolicyID determines the endorsement policy to use
func (h *Handler) determinePolicyID(req *TransferRequest) string {
	if req.AmountVND >= h.config.Transaction.HighRiskThreshold {
		return "high_risk"
	}
	if req.TransferType == TransferTypeInterBank {
		return "inter_bank_standard"
	}
	return "intra_bank_standard"
}

// receiptToDTO converts internal Receipt to API DTO
func (h *Handler) receiptToDTO(r *receipt.Receipt) *ReceiptDTO {
	dto := &ReceiptDTO{
		SchemaVersion:      r.SchemaVersion,
		ChannelID:          r.ChannelID,
		TxID:               r.TxID,
		ChaincodeID:        r.ChaincodeID,
		CommitmentHash:     r.CommitmentHash,
		BlockNumber:        r.BlockNumber,
		BlockHash:          r.BlockHash,
		TxIndex:            r.TxIndex,
		ValidationCode:     r.ValidationCode,
		ValidationCodeName: r.ValidationCodeName,
		PolicyID:           r.PolicyID,
		PolicyMet:          r.PolicyMet,
		StatusEndpoint:     r.StatusEndpoint,
		InternalRef:        r.InternalRef,
		ReceiptHash:        r.ReceiptHash,
		Timestamps: TimestampsDTO{
			ClientSubmit:    r.Timestamps.ClientSubmit,
			EndorsementTime: r.Timestamps.EndorsementTime,
			OrdererReceive:  r.Timestamps.OrdererReceive,
			BlockCommit:     r.Timestamps.BlockCommit,
			ReceiptCreated:  r.Timestamps.ReceiptCreated,
		},
	}

	for _, e := range r.Endorsements {
		dto.Endorsements = append(dto.Endorsements, EndorsementDTO{
			MSPID:           e.MSPID,
			EndorserID:      e.EndorserID,
			CertFingerprint: e.CertFingerprint,
			CertificatePEM:  e.CertificatePEM,
			SignatureHex:    e.SignatureHex,
			Timestamp:       e.Timestamp,
			SignatureValid:  e.SignatureValid,
			CertChainValid:  e.CertChainValid,
		})
	}

	return dto
}

func (h *Handler) appendCommittedEndorsements(
	buildInput *receipt.BuildInput,
	endorsements []*fabric.EndorsementInfo,
) {
	for _, e := range endorsements {
		buildInput.Endorsements = append(buildInput.Endorsements, receipt.EndorsementInput{
			MSPID:     e.MSPID,
			CertPEM:   e.Certificate,
			Signature: e.Signature,
		})
	}
}

func (h *Handler) appendSubmittedEndorsements(
	buildInput *receipt.BuildInput,
	endorsements []*fabric.Endorsement,
) {
	for _, e := range endorsements {
		buildInput.Endorsements = append(buildInput.Endorsements, receipt.EndorsementInput{
			MSPID:          e.MSPID,
			CertPEM:        e.Certificate,
			Signature:      e.Signature,
			Timestamp:      e.Timestamp,
			SignatureValid: e.SignatureValid,
			CertChainValid: e.CertChainValid,
		})
	}
}

func (h *Handler) rebuildReceiptFromLedger(
	ctx context.Context,
	existing *receipt.Receipt,
) (*receipt.Receipt, error) {
	if existing == nil || existing.TxID == "" {
		return nil, nil
	}

	txInfo, err := h.blockQuery.GetTransactionByID(ctx, existing.TxID)
	if err != nil {
		return nil, err
	}
	if len(txInfo.Endorsements) == 0 {
		return nil, nil
	}

	buildInput := &receipt.BuildInput{
		TxID:             existing.TxID,
		CommitmentHash:   existing.CommitmentHash,
		BlockNumber:      existing.BlockNumber,
		BlockHash:        existing.BlockHash,
		TxIndex:          existing.TxIndex,
		ValidationCode:   existing.ValidationCode,
		PolicyID:         existing.PolicyID,
		PolicyMet:        existing.PolicyMet,
		InternalRef:      existing.InternalRef,
		ClientSubmitTime: timeFromUnixMilli(existing.Timestamps.ClientSubmit),
		EndorsementTime:  timeFromUnixMilli(existing.Timestamps.EndorsementTime),
		CommitTime:       timeFromUnixMilli(existing.Timestamps.BlockCommit),
	}
	if txInfo.TxIndex > 0 {
		buildInput.TxIndex = txInfo.TxIndex
	}
	h.appendCommittedEndorsements(buildInput, txInfo.Endorsements)

	rebuilt, err := h.receiptBuilder.Build(buildInput)
	if err != nil {
		return nil, err
	}
	rebuilt.ReceiptSignature = existing.ReceiptSignature
	rebuilt.Timestamps.OrdererReceive = existing.Timestamps.OrdererReceive

	return rebuilt, nil
}

func timeFromUnixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// dtoToReceipt converts API DTO to internal Receipt
func (h *Handler) dtoToReceipt(dto *ReceiptDTO) *receipt.Receipt {
	r := &receipt.Receipt{
		SchemaVersion:      dto.SchemaVersion,
		ChannelID:          dto.ChannelID,
		TxID:               dto.TxID,
		ChaincodeID:        dto.ChaincodeID,
		CommitmentHash:     dto.CommitmentHash,
		BlockNumber:        dto.BlockNumber,
		BlockHash:          dto.BlockHash,
		TxIndex:            dto.TxIndex,
		ValidationCode:     dto.ValidationCode,
		ValidationCodeName: dto.ValidationCodeName,
		PolicyID:           dto.PolicyID,
		PolicyMet:          dto.PolicyMet,
		StatusEndpoint:     dto.StatusEndpoint,
		InternalRef:        dto.InternalRef,
		ReceiptHash:        dto.ReceiptHash,
		Timestamps: receipt.ReceiptTimestamps{
			ClientSubmit:    dto.Timestamps.ClientSubmit,
			EndorsementTime: dto.Timestamps.EndorsementTime,
			OrdererReceive:  dto.Timestamps.OrdererReceive,
			BlockCommit:     dto.Timestamps.BlockCommit,
			ReceiptCreated:  dto.Timestamps.ReceiptCreated,
		},
	}

	for _, e := range dto.Endorsements {
		r.Endorsements = append(r.Endorsements, receipt.EndorsementRecord{
			MSPID:           e.MSPID,
			EndorserID:      e.EndorserID,
			CertFingerprint: e.CertFingerprint,
			CertificatePEM:  e.CertificatePEM,
			SignatureHex:    e.SignatureHex,
			Timestamp:       e.Timestamp,
			SignatureValid:  e.SignatureValid,
			CertChainValid:  e.CertChainValid,
		})
	}

	return r
}

// respondError sends an error response
func (h *Handler) respondError(c *gin.Context, status int, code, message, details string) {
	c.JSON(status, TransferResponse{
		Success: false,
		Error: &ErrorDetail{
			Code:    code,
			Message: message,
			Details: details,
		},
		ProcessedAt: time.Now().UnixMilli(),
	})
}
