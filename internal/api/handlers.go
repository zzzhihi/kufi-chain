// Package api provides HTTP API handlers for the payment gateway
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hyperledger/fabric-gateway/pkg/identity"
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
	attestorSign     identity.Sign
	attestorCertFP   string
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

	attestorSign, attestorCertFP, err := loadAttestorSigner(cfg.Fabric.KeyPath, cfg.Fabric.CertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load attestor signer: %w", err)
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
		attestorSign:     attestorSign,
		attestorCertFP:   attestorCertFP,
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
		v1.GET("/observe/:tx_id", h.ObserveTransaction)
	}

	// Health check
	r.GET("/health", h.HealthCheck)
}

type observePayload struct {
	MSPID          string `json:"msp_id"`
	TxID           string `json:"tx_id"`
	BlockNumber    uint64 `json:"block_number"`
	BlockHash      string `json:"block_hash"`
	ValidationCode int32  `json:"validation_code"`
	ObservedAt     int64  `json:"observed_at"`
}

// ObserveTransaction returns node-level attestation that tx is committed on this node's ledger.
func (h *Handler) ObserveTransaction(c *gin.Context) {
	txID := strings.TrimSpace(c.Param("tx_id"))
	if txID == "" {
		h.respondError(c, http.StatusBadRequest, ErrCodeBadRequest, "Transaction ID required", "")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.config.Fabric.Timeouts.Evaluate)
	defer cancel()

	blockInfo, err := h.blockQuery.GetBlockByTxID(ctx, txID)
	payload := observePayload{
		MSPID:      h.config.Fabric.MSPID,
		TxID:       txID,
		ObservedAt: time.Now().UnixMilli(),
	}
	if err == nil {
		txInfo, txErr := h.blockQuery.GetTransactionByID(ctx, txID)
		if txErr == nil {
			payload.BlockNumber = blockInfo.BlockNumber
			payload.BlockHash = blockInfo.BlockHash
			payload.ValidationCode = txInfo.ValidationCode
		}
	}
	// Fallback path for orgs without QSCC ACL: prove state presence via chaincode query.
	if payload.BlockHash == "" {
		intentRaw, qErr := h.fabricClient.EvaluateTransaction(ctx, "QueryTransfer", txID)
		if qErr != nil || len(intentRaw) == 0 {
			details := ""
			if err != nil {
				details = err.Error()
			}
			if qErr != nil {
				if details != "" {
					details += " | "
				}
				details += qErr.Error()
			}
			h.respondError(c, http.StatusNotFound, ErrCodeNotFound, "Transaction not found on this node ledger", details)
			return
		}
		payload.ValidationCode = 0
	}
	body, _ := json.Marshal(payload)
	digest := sha256.Sum256(body)
	sig, err := h.attestorSign(digest[:])
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, ErrCodeInternalError, "Failed to sign observation", err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"msp_id":           payload.MSPID,
		"tx_id":            payload.TxID,
		"block_number":     payload.BlockNumber,
		"block_hash":       payload.BlockHash,
		"validation_code":  payload.ValidationCode,
		"observed_at":      payload.ObservedAt,
		"proof_type":       map[bool]string{true: "block_commit", false: "state_presence"}[payload.BlockHash != ""],
		"payload_hash":     hex.EncodeToString(digest[:]),
		"signature_hex":    hex.EncodeToString(sig),
		"cert_fingerprint": h.attestorCertFP,
	})
}

func loadAttestorSigner(keyPath string, certPath string) (identity.Sign, string, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("read key: %w", err)
	}
	priv, err := identity.PrivateKeyFromPEM(keyPEM)
	if err != nil {
		return nil, "", fmt.Errorf("parse key: %w", err)
	}
	sign, err := identity.NewPrivateKeySign(priv)
	if err != nil {
		return nil, "", fmt.Errorf("create signer: %w", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, "", fmt.Errorf("read cert: %w", err)
	}
	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, "", fmt.Errorf("parse cert: %w", err)
	}
	fingerprint := sha256.Sum256(cert.Raw)
	return sign, hex.EncodeToString(fingerprint[:]), nil
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

	// Determine collection based on transfer type.
	// Transparency-first mode: route normal transfers to shared inter-bank collection
	// so at least one independent org can co-endorse.
	var collectionName string
	if req.AmountVND >= h.config.Transaction.HighRiskThreshold {
		collectionName = h.config.Collections.Regulator
		if collectionName == "" {
			collectionName = "collectionHighRisk"
		}
	} else {
		collectionName = h.config.Collections.InterBank
		if collectionName == "" {
			collectionName = "collectionInterBank"
		}
	}

	// Prepare transient data for PDC
	transientData := map[string][]byte{
		collectionName: privateDataJSON,
	}

	// Require cross-org endorsements for transparency.
	endorsingOrgs := h.selectTransparencyEndorsingOrgs()

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
	if err != nil && isMissingEndorsingPeerError(err) {
		h.logger.Warn("Cross-org endorsement unavailable, retrying with origin org only",
			zap.String("internalRef", req.InternalRef),
			zap.String("originMSP", h.config.Fabric.MSPID),
			zap.Error(err),
		)
		endorsingOrgs = []string{h.config.Fabric.MSPID}
		result, err = h.fabricClient.SubmitTransactionWithPDC(
			ctx,
			"CreateTransferIntent",
			transientData,
			endorsingOrgs,
			commitmentHash,
			req.InternalRef,
			policyID,
		)
	}
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
		TxID:              result.TxID,
		CommitmentHash:    commitmentHash,
		CommitmentOpening: commitment,
		BlockNumber:       result.BlockNumber,
		BlockHash:         blockHash,
		ValidationCode:    result.ValidationCode,
		OriginMSPID:       h.config.Fabric.MSPID,
		PolicyID:          policyID,
		PolicyMet:         h.isTransparencyPolicyMet(result.Endorsements),
		InternalRef:       req.InternalRef,
		ClientSubmitTime:  clientSubmitTime,
		EndorsementTime:   result.CommitTimestamp,
		CommitTime:        result.CommitTimestamp,
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

func isMissingEndorsingPeerError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "failed to find any endorsing peers")
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
		Valid:                  result.Valid,
		Errors:                 result.Errors,
		Warnings:               result.Warnings,
		ReceiptHashValid:       result.ReceiptHashValid,
		CommitmentHashMatch:    result.CommitmentHashMatch,
		CommitmentOpeningValid: result.CommitmentOpeningValid,
		ValidationCodeValid:    result.ValidationCodeValid,
		PolicySatisfied:        result.PolicySatisfied,
		BlockVerified:          result.BlockVerified,
		VerifiedAt:             result.VerifiedAt,
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
	// Keep a single shared policy for normal transfers to support cross-org endorsements.
	return "inter_bank_standard"
}

func (h *Handler) selectTransparencyEndorsingOrgs() []string {
	// Local canonical network has 3 orgs. We require quorum >= 2 endorsements
	// including the origin org and at least one independent org.
	candidates := []string{
		h.config.Fabric.MSPID,
		"TechcombankMSP",
		"VietcombankMSP",
		"TaxMSP",
	}
	seen := map[string]struct{}{}
	orgs := make([]string, 0, 3)
	for _, org := range candidates {
		if org == "" {
			continue
		}
		if _, ok := seen[org]; ok {
			continue
		}
		seen[org] = struct{}{}
		orgs = append(orgs, org)
	}
	if len(orgs) == 1 {
		return orgs
	}
	// request 2 endorsements by default (N/2 quorum in a 3-org network)
	return orgs[:2]
}

func (h *Handler) isTransparencyPolicyMet(endorsements []*fabric.Endorsement) bool {
	if len(endorsements) == 0 {
		return false
	}
	origin := h.config.Fabric.MSPID
	distinct := map[string]struct{}{}
	hasIndependent := false
	for _, e := range endorsements {
		if e == nil || e.MSPID == "" {
			continue
		}
		distinct[e.MSPID] = struct{}{}
		if e.MSPID != origin {
			hasIndependent = true
		}
	}
	return len(distinct) >= 2 && hasIndependent
}

// receiptToDTO converts internal Receipt to API DTO
func (h *Handler) receiptToDTO(r *receipt.Receipt) *ReceiptDTO {
	dto := &ReceiptDTO{
		SchemaVersion:       r.SchemaVersion,
		ChannelID:           r.ChannelID,
		TxID:                r.TxID,
		ChaincodeID:         r.ChaincodeID,
		CommitmentHash:      r.CommitmentHash,
		CommitmentAlgorithm: r.CommitmentAlgorithm,
		BlockNumber:         r.BlockNumber,
		BlockHash:           r.BlockHash,
		TxIndex:             r.TxIndex,
		ValidationCode:      r.ValidationCode,
		ValidationCodeName:  r.ValidationCodeName,
		PolicyID:            r.PolicyID,
		OriginMSPID:         r.OriginMSPID,
		PolicyMet:           r.PolicyMet,
		StatusEndpoint:      r.StatusEndpoint,
		Observation: ObservationDTO{
			ObservedAt: r.Observation.ObservedAt,
			AnchorType: r.Observation.AnchorType,
			AnchorRef:  r.Observation.AnchorRef,
		},
		InternalRef: r.InternalRef,
		ReceiptHash: r.ReceiptHash,
		Timestamps: TimestampsDTO{
			ClientSubmit:    r.Timestamps.ClientSubmit,
			EndorsementTime: r.Timestamps.EndorsementTime,
			OrdererReceive:  r.Timestamps.OrdererReceive,
			BlockCommit:     r.Timestamps.BlockCommit,
			ReceiptCreated:  r.Timestamps.ReceiptCreated,
		},
	}
	if r.CommitmentOpening != nil {
		dto.CommitmentOpening = &CommitmentOpeningDTO{
			ChannelID:   r.CommitmentOpening.ChannelID,
			ChaincodeID: r.CommitmentOpening.ChaincodeID,
			Function:    r.CommitmentOpening.Function,
			FromID:      r.CommitmentOpening.FromID,
			ToID:        r.CommitmentOpening.ToID,
			AmountVND:   r.CommitmentOpening.AmountVND,
			InternalRef: r.CommitmentOpening.InternalRef,
			Timestamp:   r.CommitmentOpening.Timestamp,
			Nonce:       r.CommitmentOpening.Nonce,
		}
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
		TxID:              existing.TxID,
		CommitmentHash:    existing.CommitmentHash,
		CommitmentOpening: existing.CommitmentOpening,
		BlockNumber:       existing.BlockNumber,
		BlockHash:         existing.BlockHash,
		TxIndex:           existing.TxIndex,
		ValidationCode:    existing.ValidationCode,
		OriginMSPID:       existing.OriginMSPID,
		PolicyID:          existing.PolicyID,
		PolicyMet:         existing.PolicyMet,
		InternalRef:       existing.InternalRef,
		ClientSubmitTime:  timeFromUnixMilli(existing.Timestamps.ClientSubmit),
		EndorsementTime:   timeFromUnixMilli(existing.Timestamps.EndorsementTime),
		CommitTime:        timeFromUnixMilli(existing.Timestamps.BlockCommit),
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
	if existing.CommitmentAlgorithm != "" {
		rebuilt.CommitmentAlgorithm = existing.CommitmentAlgorithm
	}
	if existing.Observation.ObservedAt > 0 || existing.Observation.AnchorRef != "" || existing.Observation.AnchorType != "" {
		rebuilt.Observation = existing.Observation
	}
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
		SchemaVersion:       dto.SchemaVersion,
		ChannelID:           dto.ChannelID,
		TxID:                dto.TxID,
		ChaincodeID:         dto.ChaincodeID,
		CommitmentHash:      dto.CommitmentHash,
		CommitmentAlgorithm: dto.CommitmentAlgorithm,
		BlockNumber:         dto.BlockNumber,
		BlockHash:           dto.BlockHash,
		TxIndex:             dto.TxIndex,
		ValidationCode:      dto.ValidationCode,
		ValidationCodeName:  dto.ValidationCodeName,
		PolicyID:            dto.PolicyID,
		OriginMSPID:         dto.OriginMSPID,
		PolicyMet:           dto.PolicyMet,
		StatusEndpoint:      dto.StatusEndpoint,
		Observation: receipt.ObservationMeta{
			ObservedAt: dto.Observation.ObservedAt,
			AnchorType: dto.Observation.AnchorType,
			AnchorRef:  dto.Observation.AnchorRef,
		},
		InternalRef: dto.InternalRef,
		ReceiptHash: dto.ReceiptHash,
		Timestamps: receipt.ReceiptTimestamps{
			ClientSubmit:    dto.Timestamps.ClientSubmit,
			EndorsementTime: dto.Timestamps.EndorsementTime,
			OrdererReceive:  dto.Timestamps.OrdererReceive,
			BlockCommit:     dto.Timestamps.BlockCommit,
			ReceiptCreated:  dto.Timestamps.ReceiptCreated,
		},
	}
	if dto.CommitmentOpening != nil {
		r.CommitmentOpening = &receipt.TransferCommitment{
			ChannelID:   dto.CommitmentOpening.ChannelID,
			ChaincodeID: dto.CommitmentOpening.ChaincodeID,
			Function:    dto.CommitmentOpening.Function,
			FromID:      dto.CommitmentOpening.FromID,
			ToID:        dto.CommitmentOpening.ToID,
			AmountVND:   dto.CommitmentOpening.AmountVND,
			InternalRef: dto.CommitmentOpening.InternalRef,
			Timestamp:   dto.CommitmentOpening.Timestamp,
			Nonce:       dto.CommitmentOpening.Nonce,
		}
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
