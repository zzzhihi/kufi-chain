// Package receipt provides receipt verification functionality
package receipt

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// ecdsaSignature represents an ASN.1 encoded ECDSA signature
type ecdsaSignature struct {
	R, S *big.Int
}

// VerificationResult contains the result of receipt verification
type VerificationResult struct {
	Valid                  bool                      `json:"valid"`
	Errors                 []string                  `json:"errors,omitempty"`
	Warnings               []string                  `json:"warnings,omitempty"`
	ReceiptHashValid       bool                      `json:"receipt_hash_valid"`
	CommitmentHashMatch    bool                      `json:"commitment_hash_match,omitempty"`
	CommitmentOpeningValid bool                      `json:"commitment_opening_valid,omitempty"`
	ValidationCodeValid    bool                      `json:"validation_code_valid"`
	PolicySatisfied        bool                      `json:"policy_satisfied"`
	EndorsementResults     []EndorsementVerifyResult `json:"endorsement_results"`
	BlockVerified          bool                      `json:"block_verified,omitempty"`
	VerifiedAt             int64                     `json:"verified_at"`
}

// EndorsementVerifyResult contains verification result for single endorsement
type EndorsementVerifyResult struct {
	MSPID          string `json:"msp_id"`
	EndorserID     string `json:"endorser_id"`
	SignatureValid bool   `json:"signature_valid"`
	CertChainValid bool   `json:"cert_chain_valid"`
	CertExpired    bool   `json:"cert_expired"`
	CertRevoked    bool   `json:"cert_revoked"`
	Error          string `json:"error,omitempty"`
}

// Verifier verifies transaction receipts
type Verifier struct {
	trustedCAs        map[string]*x509.CertPool // MSP ID -> CA pool
	policyConfig      map[string]PolicyConfig   // Policy ID -> config
	crlEndpoint       string                    // CRL/OCSP endpoint
	allowExpiredCerts bool                      // For testing
}

// PolicyConfig defines endorsement policy requirements
type PolicyConfig struct {
	Type         string // "AND", "OR", "MAJORITY"
	RequiredMSPs []string
	MinEndorsers int
}

// NewVerifier creates a new receipt verifier
func NewVerifier() *Verifier {
	return &Verifier{
		trustedCAs:   make(map[string]*x509.CertPool),
		policyConfig: make(map[string]PolicyConfig),
	}
}

// AddTrustedCA adds a trusted CA certificate for an MSP
func (v *Verifier) AddTrustedCA(mspID string, caCertPEM []byte) error {
	block, _ := pem.Decode(caCertPEM)
	if block == nil {
		return fmt.Errorf("failed to decode CA certificate PEM for MSP %s", mspID)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	pool, exists := v.trustedCAs[mspID]
	if !exists {
		pool = x509.NewCertPool()
		v.trustedCAs[mspID] = pool
	}

	pool.AddCert(cert)
	return nil
}

// AddPolicyConfig adds an endorsement policy configuration
func (v *Verifier) AddPolicyConfig(policyID string, config PolicyConfig) {
	v.policyConfig[policyID] = config
}

// SetCRLEndpoint sets the CRL/revocation check endpoint
func (v *Verifier) SetCRLEndpoint(endpoint string) {
	v.crlEndpoint = endpoint
}

// SetAllowExpiredCerts allows expired certificates (for testing only)
func (v *Verifier) SetAllowExpiredCerts(allow bool) {
	v.allowExpiredCerts = allow
}

// VerifyReceipt performs comprehensive verification of a receipt
func (v *Verifier) VerifyReceipt(receipt *Receipt, expectedCommitmentHash string) (*VerificationResult, error) {
	result := &VerificationResult{
		Valid:      true,
		VerifiedAt: time.Now().UnixMilli(),
	}

	// 1. Verify receipt hash integrity
	computedHash := receipt.ComputeReceiptHash()
	result.ReceiptHashValid = (computedHash == receipt.ReceiptHash)
	if !result.ReceiptHashValid {
		result.Valid = false
		result.Errors = append(result.Errors, "receipt hash mismatch - receipt may have been tampered with")
	}

	// 2. Verify commitment hash if provided
	if expectedCommitmentHash != "" {
		result.CommitmentHashMatch = (receipt.CommitmentHash == expectedCommitmentHash)
		if !result.CommitmentHashMatch {
			result.Valid = false
			result.Errors = append(result.Errors, "commitment hash does not match expected value")
		}
	}

	// 2b. Verify commitment opening if present (self-recomputable path)
	if receipt.CommitmentOpening != nil {
		recomputed := ComputeCommitmentHash(receipt.CommitmentOpening)
		result.CommitmentOpeningValid = (recomputed == receipt.CommitmentHash)
		if !result.CommitmentOpeningValid {
			result.Valid = false
			result.Errors = append(result.Errors, "commitment opening does not match commitment hash")
		}
	} else {
		result.Warnings = append(result.Warnings, "missing commitment opening; cannot independently recompute commitment hash")
	}

	// 3. Verify validation code indicates success
	result.ValidationCodeValid = (receipt.ValidationCode == 0) // VALID = 0
	if !result.ValidationCodeValid {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("transaction validation failed: %s", receipt.ValidationCodeName))
	}

	// 4. Verify each endorsement
	validMSPs := make(map[string]bool)
	for _, endorsement := range receipt.Endorsements {
		evResult := v.verifyEndorsement(endorsement)
		result.EndorsementResults = append(result.EndorsementResults, evResult)

		if evResult.SignatureValid && evResult.CertChainValid && !evResult.CertExpired && !evResult.CertRevoked {
			validMSPs[endorsement.MSPID] = true
		}
	}

	// 5. Verify endorsement policy
	result.PolicySatisfied = v.verifyPolicy(receipt.PolicyID, validMSPs)
	if !result.PolicySatisfied && v.canTrustLedgerValidationFallback(receipt, result.EndorsementResults) {
		result.PolicySatisfied = true
		result.Warnings = append(result.Warnings,
			"endorsement crypto proof is incomplete in receipt; policy accepted based on committed VALID transaction")
	}
	if !result.PolicySatisfied {
		result.Valid = false
		result.Errors = append(result.Errors, fmt.Sprintf("endorsement policy '%s' not satisfied", receipt.PolicyID))
	}

	// 6. Additional checks
	if receipt.BlockNumber == 0 {
		result.Warnings = append(result.Warnings, "block number is 0, might indicate incomplete receipt")
	}

	if len(receipt.Endorsements) == 0 {
		result.Valid = false
		result.Errors = append(result.Errors, "receipt contains no endorsements")
	}

	return result, nil
}

// verifyEndorsement verifies a single endorsement
func (v *Verifier) verifyEndorsement(endorsement EndorsementRecord) EndorsementVerifyResult {
	result := EndorsementVerifyResult{
		MSPID:      endorsement.MSPID,
		EndorserID: endorsement.EndorserID,
	}

	// If we have the full certificate, perform full verification
	if endorsement.CertificatePEM != "" {
		block, _ := pem.Decode([]byte(endorsement.CertificatePEM))
		if block == nil {
			result.Error = "failed to decode certificate PEM"
			return result
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			result.Error = fmt.Sprintf("failed to parse certificate: %v", err)
			return result
		}

		// Check certificate expiration
		now := time.Now()
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			result.CertExpired = true
			if !v.allowExpiredCerts {
				result.Error = "certificate expired or not yet valid"
			}
		}

		// Verify certificate chain against trusted CA
		if caPool, ok := v.trustedCAs[endorsement.MSPID]; ok {
			opts := x509.VerifyOptions{
				Roots:       caPool,
				CurrentTime: now,
			}
			if v.allowExpiredCerts {
				opts.CurrentTime = cert.NotBefore.Add(time.Hour)
			}
			_, err := cert.Verify(opts)
			result.CertChainValid = (err == nil)
			if err != nil && result.Error == "" {
				result.Error = fmt.Sprintf("certificate chain verification failed: %v", err)
			}
		} else {
			// No trusted CA configured, use claimed status
			result.CertChainValid = endorsement.CertChainValid
			result.Error = fmt.Sprintf("no trusted CA configured for MSP %s", endorsement.MSPID)
		}

		// Verify certificate fingerprint matches
		hash := sha256.Sum256(block.Bytes)
		expectedFingerprint := hex.EncodeToString(hash[:])
		if endorsement.CertFingerprint != expectedFingerprint {
			result.Error = "certificate fingerprint mismatch"
			result.CertChainValid = false
		}

		// Signature verification would require the original proposal response bytes
		// For offline verification, we rely on the recorded status
		result.SignatureValid = endorsement.SignatureValid
	} else {
		// No full certificate, use recorded verification status
		result.SignatureValid = endorsement.SignatureValid
		result.CertChainValid = endorsement.CertChainValid
	}

	// Check for revocation if endpoint configured
	if v.crlEndpoint != "" && result.CertChainValid {
		revoked, err := v.checkRevocation(endorsement.CertFingerprint)
		if err != nil {
			result.Error = fmt.Sprintf("revocation check failed: %v", err)
		}
		result.CertRevoked = revoked
	}

	return result
}

// verifyPolicy checks if the endorsement policy is satisfied
func (v *Verifier) verifyPolicy(policyID string, validMSPs map[string]bool) bool {
	config, ok := v.policyConfig[policyID]
	if !ok {
		// No policy config, check if at least one valid endorsement
		return len(validMSPs) > 0
	}

	switch config.Type {
	case "AND":
		for _, msp := range config.RequiredMSPs {
			if !validMSPs[msp] {
				return false
			}
		}
		return true

	case "OR":
		for _, msp := range config.RequiredMSPs {
			if validMSPs[msp] {
				return true
			}
		}
		return false

	case "MAJORITY":
		count := 0
		for _, msp := range config.RequiredMSPs {
			if validMSPs[msp] {
				count++
			}
		}
		return count > len(config.RequiredMSPs)/2

	case "MIN":
		return len(validMSPs) >= config.MinEndorsers

	default:
		return len(validMSPs) > 0
	}
}

func (v *Verifier) canTrustLedgerValidationFallback(
	receipt *Receipt,
	endorsementResults []EndorsementVerifyResult,
) bool {
	if receipt == nil || receipt.ValidationCode != 0 || len(receipt.Endorsements) == 0 {
		return false
	}

	// If at least one endorsement already has explicit cryptographic validity,
	// normal policy evaluation should apply (no fallback).
	for _, er := range endorsementResults {
		if er.SignatureValid && er.CertChainValid && !er.CertExpired && !er.CertRevoked {
			return false
		}
	}

	// Fallback is allowed only when receipt lacks enough crypto material
	// (no cert PEM and no positive signature/chain flags).
	for _, e := range receipt.Endorsements {
		if e.CertificatePEM != "" || e.SignatureValid || e.CertChainValid {
			return false
		}
	}

	return true
}

// checkRevocation checks if a certificate is revoked
func (v *Verifier) checkRevocation(certFingerprint string) (bool, error) {
	// In production, this would:
	// 1. Check local CRL cache
	// 2. Query OCSP responder
	// 3. Download and check CRL from configured endpoint

	// Placeholder implementation
	return false, nil
}

// VerifySignature verifies an ECDSA signature against a certificate and message
func VerifySignature(certPEM []byte, signatureHex string, messageHash []byte) (bool, error) {
	// Parse certificate
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false, fmt.Errorf("failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Get public key
	pubKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return false, fmt.Errorf("certificate public key is not ECDSA")
	}

	// Decode signature
	sigBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature hex: %w", err)
	}

	// Parse ASN.1 signature
	r, s, err := parseASN1Signature(sigBytes)
	if err != nil {
		return false, fmt.Errorf("failed to parse signature: %w", err)
	}

	// Verify
	return ecdsa.Verify(pubKey, messageHash, r, s), nil
}

// parseASN1Signature parses an ASN.1 DER encoded ECDSA signature using stdlib
func parseASN1Signature(sig []byte) (*big.Int, *big.Int, error) {
	var esig ecdsaSignature
	if _, err := asn1.Unmarshal(sig, &esig); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal ASN.1 signature: %w", err)
	}
	if esig.R == nil || esig.S == nil {
		return nil, nil, fmt.Errorf("invalid signature: R or S is nil")
	}
	return esig.R, esig.S, nil
}

// QuickVerify performs a quick verification without full cryptographic checks
// Useful for initial validation before expensive operations
func QuickVerify(receipt *Receipt) error {
	// Check required fields
	if receipt.TxID == "" {
		return fmt.Errorf("missing transaction ID")
	}
	if receipt.ChannelID == "" {
		return fmt.Errorf("missing channel ID")
	}
	if receipt.CommitmentHash == "" {
		return fmt.Errorf("missing commitment hash")
	}
	if receipt.SchemaVersion == "" {
		return fmt.Errorf("missing schema version")
	}

	// Check receipt hash
	computed := receipt.ComputeReceiptHash()
	if computed != receipt.ReceiptHash {
		return fmt.Errorf("receipt hash mismatch")
	}

	// Check validation code
	if receipt.ValidationCode != 0 {
		return fmt.Errorf("transaction not valid: %s", receipt.ValidationCodeName)
	}

	// Check policy met flag
	if !receipt.PolicyMet {
		return fmt.Errorf("endorsement policy not satisfied")
	}

	// Check endorsements exist
	if len(receipt.Endorsements) == 0 {
		return fmt.Errorf("no endorsements in receipt")
	}

	return nil
}
