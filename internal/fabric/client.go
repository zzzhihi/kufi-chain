// Package fabric provides Hyperledger Fabric Gateway SDK wrapper
package fabric

import (
	"context"
	"crypto/x509"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-gateway/pkg/identity"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/gateway"
	"github.com/hyperledger/fabric-protos-go-apiv2/msp"
	fabpeer "github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fabric-payment-gateway/internal/config"
)

// Client wraps Fabric Gateway SDK operations
type Client struct {
	config    *config.FabricConfig
	gateway   *client.Gateway
	network   *client.Network
	contract  *client.Contract
	grpcConn  *grpc.ClientConn
	logger    *zap.Logger
	mu        sync.RWMutex
	connected bool
	sem       chan struct{} // concurrency semaphore for Fabric calls
}

// NewClient creates a new Fabric client
func NewClient(cfg *config.FabricConfig, logger *zap.Logger) *Client {
	maxConcurrent := 200 // default max concurrent Fabric submissions
	if cfg.MaxConcurrent > 0 {
		maxConcurrent = cfg.MaxConcurrent
	}
	return &Client{
		config: cfg,
		logger: logger,
		sem:    make(chan struct{}, maxConcurrent),
	}
}

// Connect establishes connection to Fabric network
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	c.logger.Info("Connecting to Fabric network",
		zap.String("mspID", c.config.MSPID),
		zap.String("channel", c.config.ChannelName),
	)

	// Load identity
	id, err := c.loadIdentity()
	if err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}

	// Load signing key
	sign, err := c.loadSign()
	if err != nil {
		return fmt.Errorf("failed to load signing key: %w", err)
	}

	// Create gRPC connection - try all configured peers with failover
	if len(c.config.Peers) == 0 {
		return fmt.Errorf("no peers configured")
	}

	var conn *grpc.ClientConn
	var connErr error
	for i, peer := range c.config.Peers {
		conn, connErr = c.createGRPCConnection(ctx, peer)
		if connErr == nil {
			c.logger.Info("Connected to peer",
				zap.String("endpoint", peer.Endpoint),
				zap.Int("peerIndex", i),
			)
			break
		}
		c.logger.Warn("Failed to connect to peer, trying next",
			zap.String("endpoint", peer.Endpoint),
			zap.Int("peerIndex", i),
			zap.Error(connErr),
		)
	}
	if connErr != nil {
		return fmt.Errorf("failed to connect to any peer: %w", connErr)
	}
	c.grpcConn = conn

	// Create Gateway connection
	gw, err := client.Connect(
		id,
		client.WithSign(sign),
		client.WithClientConnection(conn),
		client.WithEvaluateTimeout(c.config.Timeouts.Evaluate),
		client.WithEndorseTimeout(c.config.Timeouts.Endorse),
		client.WithSubmitTimeout(c.config.Timeouts.Submit),
		client.WithCommitStatusTimeout(c.config.Timeouts.CommitStatus),
	)
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to connect gateway: %w", err)
	}
	c.gateway = gw

	// Get network and contract
	c.network = gw.GetNetwork(c.config.ChannelName)
	c.contract = c.network.GetContract(c.config.Chaincode.Name)
	c.connected = true

	c.logger.Info("Successfully connected to Fabric network")
	return nil
}

// Close closes the Fabric connection
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.gateway != nil {
		c.gateway.Close()
	}
	if c.grpcConn != nil {
		c.grpcConn.Close()
	}
	c.connected = false
	c.logger.Info("Fabric connection closed")
}

// IsConnected returns connection status
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// loadIdentity loads the X.509 certificate identity
func (c *Client) loadIdentity() (*identity.X509Identity, error) {
	certPEM, err := os.ReadFile(c.config.CertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read certificate: %w", err)
	}

	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return identity.NewX509Identity(c.config.MSPID, cert)
}

// loadSign loads the signing function from private key
func (c *Client) loadSign() (identity.Sign, error) {
	keyPEM, err := os.ReadFile(c.config.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}

	privateKey, err := identity.PrivateKeyFromPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	return identity.NewPrivateKeySign(privateKey)
}

// createGRPCConnection creates a gRPC connection to peer
func (c *Client) createGRPCConnection(ctx context.Context, peer config.PeerConfig) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	if c.config.TLS.Enabled {
		// Load TLS certificate
		certPEM, err := os.ReadFile(peer.TLSCertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read TLS cert: %w", err)
		}

		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(certPEM) {
			return nil, fmt.Errorf("failed to add TLS cert to pool")
		}

		tlsCreds := credentials.NewClientTLSFromCert(certPool, peer.OverrideAuthority)
		opts = append(opts, grpc.WithTransportCredentials(tlsCreds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.DialContext(context.Background(), peer.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create client for peer: %w", err)
	}

	return conn, nil
}

// GetContract returns the chaincode contract
func (c *Client) GetContract() *client.Contract {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.contract
}

// GetNetwork returns the network instance
func (c *Client) GetNetwork() *client.Network {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.network
}

// SubmitResult contains the result of a transaction submission
type SubmitResult struct {
	TxID            string
	BlockNumber     uint64
	ValidationCode  int32
	Endorsements    []*Endorsement
	CommitTimestamp time.Time
	Payload         []byte
}

// Endorsement represents an endorser's signature
type Endorsement struct {
	MSPID       string
	Signature   []byte
	Certificate []byte
	Timestamp   time.Time
	// Verification results populated by EndorsementExtractor
	SignatureValid bool
	CertChainValid bool
}

// SubmitTransaction submits a transaction and waits for commit.
// Uses retry with exponential backoff for transient failures.
func (c *Client) SubmitTransaction(ctx context.Context, fn string, args ...string) (*SubmitResult, error) {
	return c.submitWithRetry(ctx, func(contract *client.Contract) (*SubmitResult, error) {
		c.logger.Debug("Submitting transaction",
			zap.String("function", fn),
			zap.Strings("args", args),
		)

		// Create proposal
		proposal, err := contract.NewProposal(fn, client.WithArguments(args...))
		if err != nil {
			return nil, fmt.Errorf("failed to create proposal: %w", err)
		}

		// Endorse proposal
		txn, err := proposal.Endorse()
		if err != nil {
			return nil, fmt.Errorf("failed to endorse: %s", formatGatewayError(err))
		}

		// Submit endorsed transaction
		commit, err := txn.Submit()
		if err != nil {
			return nil, fmt.Errorf("failed to submit: %w", err)
		}

		// Wait for commit
		st, err := commit.Status()
		if err != nil {
			return nil, fmt.Errorf("failed to get commit status: %w", err)
		}

		if !st.Successful {
			return nil, fmt.Errorf("transaction %s failed with code %d", txn.TransactionID(), st.Code)
		}

		result := &SubmitResult{
			TxID:            txn.TransactionID(),
			BlockNumber:     st.BlockNumber,
			ValidationCode:  int32(st.Code),
			CommitTimestamp: time.Now(),
			Payload:         txn.Result(),
		}
		result.Endorsements = c.extractEndorsements(txn)

		c.logger.Info("Transaction committed successfully",
			zap.String("txID", result.TxID),
			zap.Uint64("blockNumber", result.BlockNumber),
			zap.Int("endorsements", len(result.Endorsements)),
		)

		return result, nil
	})
}

// SubmitTransactionWithPDC submits transaction with private data.
// endorsingOrgs restricts endorsement to the given MSP IDs (required for per-org PDC).
// Uses retry with exponential backoff for transient failures.
func (c *Client) SubmitTransactionWithPDC(ctx context.Context, fn string, transientData map[string][]byte, endorsingOrgs []string, args ...string) (*SubmitResult, error) {
	return c.submitWithRetry(ctx, func(contract *client.Contract) (*SubmitResult, error) {
		c.logger.Debug("Submitting transaction with private data",
			zap.String("function", fn),
			zap.Strings("endorsingOrgs", endorsingOrgs),
		)

		// Create proposal with transient data and endorsing orgs
		opts := []client.ProposalOption{
			client.WithArguments(args...),
			client.WithTransient(transientData),
		}
		if len(endorsingOrgs) > 0 {
			opts = append(opts, client.WithEndorsingOrganizations(endorsingOrgs...))
		}
		proposal, err := contract.NewProposal(
			fn,
			opts...,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create proposal: %w", err)
		}

		// Endorse
		txn, err := proposal.Endorse()
		if err != nil {
			return nil, fmt.Errorf("failed to endorse: %s", formatGatewayError(err))
		}

		// Submit
		commit, err := txn.Submit()
		if err != nil {
			return nil, fmt.Errorf("failed to submit: %w", err)
		}

		// Wait for commit
		st, err := commit.Status()
		if err != nil {
			return nil, fmt.Errorf("failed to get commit status: %w", err)
		}

		if !st.Successful {
			return nil, fmt.Errorf("transaction %s failed with code %d", txn.TransactionID(), st.Code)
		}

		result := &SubmitResult{
			TxID:            txn.TransactionID(),
			BlockNumber:     st.BlockNumber,
			ValidationCode:  int32(st.Code),
			CommitTimestamp: time.Now(),
			Payload:         txn.Result(),
		}
		result.Endorsements = c.extractEndorsements(txn)

		c.logger.Info("Transaction with PDC committed",
			zap.String("txID", result.TxID),
			zap.Uint64("blockNumber", result.BlockNumber),
			zap.Int("endorsements", len(result.Endorsements)),
		)

		return result, nil
	})
}

// submitWithRetry runs submitFn with exponential backoff on transient failures.
func (c *Client) submitWithRetry(ctx context.Context, submitFn func(*client.Contract) (*SubmitResult, error)) (*SubmitResult, error) {
	// Acquire semaphore to limit concurrent Fabric calls
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("context cancelled waiting for semaphore: %w", ctx.Err())
	}

	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not connected")
	}
	contract := c.contract
	retryCfg := c.config.Retry
	c.mu.RUnlock()

	maxAttempts := retryCfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	backoff := retryCfg.InitialBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	maxBackoff := retryCfg.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Second
	}
	multiplier := retryCfg.BackoffMultiplier
	if multiplier <= 0 {
		multiplier = 2.0
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := submitFn(contract)
		if err == nil {
			return result, nil
		}
		lastErr = err

		if !isRetryable(err) || attempt == maxAttempts {
			break
		}

		c.logger.Warn("Transient Fabric error, retrying",
			zap.Int("attempt", attempt),
			zap.Int("maxAttempts", maxAttempts),
			zap.Duration("backoff", backoff),
			zap.Error(err),
		)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
		}

		// Exponential backoff
		backoff = time.Duration(math.Min(
			float64(backoff)*multiplier,
			float64(maxBackoff),
		))
	}

	return nil, lastErr
}

// isRetryable returns true if the error is a transient failure worth retrying.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	retryPatterns := []string{
		"unavailable",
		"deadline exceeded",
		"connection refused",
		"transport is closing",
		"mvcc_read_conflict",
		"phantom_read_conflict",
		"service is closing",
		"too many requests",
		"exceeding concurrency",
		"resource exhausted",
	}
	for _, p := range retryPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// extractEndorsements parses the endorsed transaction envelope protobuf
// to extract real endorser MSP IDs, certificates, and signatures.
func (c *Client) extractEndorsements(txn *client.Transaction) []*Endorsement {
	envBytes, err := txn.Bytes()
	if err != nil {
		c.logger.Warn("Failed to get transaction envelope bytes", zap.Error(err))
		return nil
	}

	// Parse Envelope
	envelope := &common.Envelope{}
	if err := proto.Unmarshal(envBytes, envelope); err != nil {
		c.logger.Warn("Failed to unmarshal envelope", zap.Error(err))
		return nil
	}

	// Parse Payload
	payload := &common.Payload{}
	if err := proto.Unmarshal(envelope.GetPayload(), payload); err != nil {
		c.logger.Warn("Failed to unmarshal payload", zap.Error(err))
		return nil
	}

	// Parse Transaction
	tx := &fabpeer.Transaction{}
	if err := proto.Unmarshal(payload.GetData(), tx); err != nil {
		c.logger.Warn("Failed to unmarshal transaction", zap.Error(err))
		return nil
	}

	var endorsements []*Endorsement

	for _, action := range tx.GetActions() {
		// Parse ChaincodeActionPayload
		ccPayload := &fabpeer.ChaincodeActionPayload{}
		if err := proto.Unmarshal(action.GetPayload(), ccPayload); err != nil {
			c.logger.Warn("Failed to unmarshal chaincode action payload", zap.Error(err))
			continue
		}

		endorsedAction := ccPayload.GetAction()
		if endorsedAction == nil {
			continue
		}

		for _, e := range endorsedAction.GetEndorsements() {
			// Parse the endorser identity (SerializedIdentity)
			sid := &msp.SerializedIdentity{}
			if err := proto.Unmarshal(e.GetEndorser(), sid); err != nil {
				c.logger.Warn("Failed to unmarshal endorser identity", zap.Error(err))
				continue
			}

			endorsements = append(endorsements, &Endorsement{
				MSPID:       sid.GetMspid(),
				Certificate: sid.GetIdBytes(),
				Signature:   e.GetSignature(),
				// Fabric endorsements don't carry individual timestamps;
				// the transaction proposal timestamp is the relevant one.
				// Leave zero; callers should use commit timestamp instead.
			})

			c.logger.Debug("Extracted endorsement",
				zap.String("mspID", sid.GetMspid()),
				zap.Int("sigLen", len(e.GetSignature())),
			)
		}
	}

	return endorsements
}

// EvaluateTransaction evaluates (queries) a transaction without committing
func (c *Client) EvaluateTransaction(ctx context.Context, fn string, args ...string) ([]byte, error) {
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not connected")
	}
	contract := c.contract
	c.mu.RUnlock()

	result, err := contract.EvaluateTransaction(fn, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate: %w", err)
	}

	return result, nil
}

// QueryPrivateData queries private data from a collection
func (c *Client) QueryPrivateData(ctx context.Context, collection, fn string, args ...string) ([]byte, error) {
	c.mu.RLock()
	if !c.connected {
		c.mu.RUnlock()
		return nil, fmt.Errorf("client not connected")
	}
	contract := c.contract
	c.mu.RUnlock()

	// Create evaluation proposal targeting the private data collection
	proposal, err := contract.NewProposal(fn, client.WithArguments(args...))
	if err != nil {
		return nil, fmt.Errorf("failed to create proposal: %w", err)
	}

	result, err := proposal.Evaluate()
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate: %w", err)
	}

	return result, nil
}

// RegisterCommitListener registers a listener for commit events
func (c *Client) RegisterCommitListener(ctx context.Context, txID string) (<-chan *gateway.CommitStatusResponse, error) {
	// This would be implemented using the gateway's commit status service
	// For now, return a channel that receives the status
	ch := make(chan *gateway.CommitStatusResponse, 1)

	go func() {
		defer close(ch)
		// In production, use actual event listening
		// This is a placeholder
	}()

	return ch, nil
}

func formatGatewayError(err error) string {
	if err == nil {
		return ""
	}

	st, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}

	detailMessages := make([]string, 0, len(st.Details()))
	for _, detail := range st.Details() {
		switch d := detail.(type) {
		case *gateway.ErrorDetail:
			parts := make([]string, 0, 3)
			if d.GetMspId() != "" {
				parts = append(parts, "msp="+d.GetMspId())
			}
			if d.GetAddress() != "" {
				parts = append(parts, "peer="+d.GetAddress())
			}
			if d.GetMessage() != "" {
				parts = append(parts, d.GetMessage())
			}
			if len(parts) > 0 {
				detailMessages = append(detailMessages, strings.Join(parts, " "))
			}
		default:
			detailMessages = append(detailMessages, fmt.Sprintf("%v", detail))
		}
	}

	if len(detailMessages) == 0 {
		return st.Message()
	}

	return st.Message() + " | " + strings.Join(detailMessages, " | ")
}
