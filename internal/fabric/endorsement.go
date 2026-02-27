// Package fabric provides endorsement extraction and verification
package fabric

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"

	"go.uber.org/zap"
)

// ecdsaSignature represents an ASN.1 encoded ECDSA signature
type ecdsaSignature struct {
	R, S *big.Int
}

// EndorsementExtractor extracts and verifies endorsements from transactions
type EndorsementExtractor struct {
	logger       *zap.Logger
	trustedCAs   map[string]*x509.CertPool // MSP ID -> CA cert pool
	trustedRoots []*x509.Certificate
}

// NewEndorsementExtractor creates a new extractor with trusted CA certs
func NewEndorsementExtractor(logger *zap.Logger) *EndorsementExtractor {
	return &EndorsementExtractor{
		logger:     logger,
		trustedCAs: make(map[string]*x509.CertPool),
	}
}

// AddTrustedCA adds a trusted CA certificate for an MSP
func (ee *EndorsementExtractor) AddTrustedCA(mspID string, caCertPEM []byte) error {
	block, _ := pem.Decode(caCertPEM)
	if block == nil {
		return fmt.Errorf("failed to decode PEM block for MSP %s", mspID)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate for MSP %s: %w", mspID, err)
	}

	pool, exists := ee.trustedCAs[mspID]
	if !exists {
		pool = x509.NewCertPool()
		ee.trustedCAs[mspID] = pool
	}

	pool.AddCert(cert)
	ee.trustedRoots = append(ee.trustedRoots, cert)
	ee.logger.Info("Added trusted CA for MSP", zap.String("mspID", mspID))

	return nil
}

// VerifiedEndorsement represents a verified endorsement
type VerifiedEndorsement struct {
	MSPID            string
	EndorserCert     *x509.Certificate
	EndorserCertPEM  []byte
	Signature        []byte
	SignatureValid   bool
	CertChainValid   bool
	EndorserIdentity string
}

// VerifyEndorsement verifies an endorsement signature and certificate chain
func (ee *EndorsementExtractor) VerifyEndorsement(
	endorsement *EndorsementInfo,
	proposalResponseHash []byte,
) (*VerifiedEndorsement, error) {
	result := &VerifiedEndorsement{
		MSPID:     endorsement.MSPID,
		Signature: endorsement.Signature,
	}

	// Parse endorser certificate
	block, _ := pem.Decode(endorsement.Certificate)
	if block == nil {
		return nil, fmt.Errorf("failed to decode endorser certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse endorser certificate: %w", err)
	}

	result.EndorserCert = cert
	result.EndorserCertPEM = endorsement.Certificate
	result.EndorserIdentity = cert.Subject.CommonName

	// Verify certificate chain against trusted CA
	caPool, exists := ee.trustedCAs[endorsement.MSPID]
	if !exists {
		ee.logger.Warn("No trusted CA found for MSP",
			zap.String("mspID", endorsement.MSPID))
		result.CertChainValid = false
	} else {
		opts := x509.VerifyOptions{
			Roots: caPool,
		}
		_, err := cert.Verify(opts)
		result.CertChainValid = (err == nil)
		if err != nil {
			ee.logger.Warn("Certificate chain verification failed",
				zap.String("mspID", endorsement.MSPID),
				zap.Error(err))
		}
	}

	// Verify signature
	result.SignatureValid = ee.verifyECDSASignature(cert, endorsement.Signature, proposalResponseHash)

	return result, nil
}

// verifyECDSASignature verifies an ECDSA signature
func (ee *EndorsementExtractor) verifyECDSASignature(cert *x509.Certificate, signature, message []byte) bool {
	pubKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		ee.logger.Error("Certificate public key is not ECDSA")
		return false
	}

	// Hash the message (proposal response bytes)
	hash := sha256.Sum256(message)

	// Parse signature (ASN.1 DER encoded)
	r, s, err := parseECDSASignature(signature)
	if err != nil {
		ee.logger.Error("Failed to parse ECDSA signature", zap.Error(err))
		return false
	}

	// Verify
	return ecdsa.Verify(pubKey, hash[:], r, s)
}

// parseECDSASignature parses an ASN.1 DER encoded ECDSA signature using stdlib
func parseECDSASignature(sig []byte) (*big.Int, *big.Int, error) {
	var esig ecdsaSignature
	if _, err := asn1.Unmarshal(sig, &esig); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal ASN.1 signature: %w", err)
	}
	if esig.R == nil || esig.S == nil {
		return nil, nil, fmt.Errorf("invalid signature: R or S is nil")
	}
	return esig.R, esig.S, nil
}

// VerifyEndorsementPolicy verifies that endorsements satisfy the policy
func (ee *EndorsementExtractor) VerifyEndorsementPolicy(
	endorsements []*VerifiedEndorsement,
	policyType string,
	requiredMSPs []string,
) (bool, error) {
	validMSPs := make(map[string]bool)

	for _, e := range endorsements {
		if e.SignatureValid && e.CertChainValid {
			validMSPs[e.MSPID] = true
		}
	}

	switch policyType {
	case "AND":
		// All required MSPs must have valid endorsements
		for _, msp := range requiredMSPs {
			if !validMSPs[msp] {
				return false, fmt.Errorf("missing valid endorsement from MSP: %s", msp)
			}
		}
		return true, nil

	case "OR":
		// At least one required MSP must have valid endorsement
		for _, msp := range requiredMSPs {
			if validMSPs[msp] {
				return true, nil
			}
		}
		return false, fmt.Errorf("no valid endorsement from any required MSP")

	case "MAJORITY":
		// Majority of required MSPs must have valid endorsements
		count := 0
		for _, msp := range requiredMSPs {
			if validMSPs[msp] {
				count++
			}
		}
		majority := len(requiredMSPs)/2 + 1
		if count >= majority {
			return true, nil
		}
		return false, fmt.Errorf("insufficient endorsements: %d of %d required", count, majority)

	default:
		return false, fmt.Errorf("unknown policy type: %s", policyType)
	}
}

// ExtractEndorsementsFromProposal extracts endorsements from a proposal response
// This is a placeholder - actual implementation would parse protobuf
func (ee *EndorsementExtractor) ExtractEndorsementsFromProposal(proposalResponseBytes []byte) ([]*EndorsementInfo, error) {
	// In production, this would:
	// 1. Parse the ProposalResponse protobuf
	// 2. Extract the endorsements array
	// 3. For each endorsement, extract MSPID, signature, and certificate

	ee.logger.Debug("Extracting endorsements from proposal response")

	// Placeholder return
	return []*EndorsementInfo{}, nil
}

// GetCertificateFingerprint returns the SHA256 fingerprint of a certificate
func GetCertificateFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM")
	}

	hash := sha256.Sum256(block.Bytes)
	return fmt.Sprintf("%x", hash), nil
}
