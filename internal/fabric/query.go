// Package fabric provides block and transaction query utilities
package fabric

import (
	"context"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/msp"
	fabpeer "github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// BlockInfo contains block metadata
type BlockInfo struct {
	BlockNumber  uint64
	BlockHash    string
	PreviousHash string
	DataHash     string
	TxCount      int
	Timestamp    int64
}

// TransactionInfo contains transaction details from block
type TransactionInfo struct {
	TxID           string
	ValidationCode int32
	BlockNumber    uint64
	TxIndex        int
	ChannelID      string
	ChaincodeID    string
	Endorsements   []*EndorsementInfo
	CreatorMSPID   string
	CreatorCert    []byte
	Timestamp      int64
}

// EndorsementInfo contains endorser information
type EndorsementInfo struct {
	MSPID       string
	Signature   []byte
	Certificate []byte
}

// BlockQuery provides block and transaction query operations
type BlockQuery struct {
	client *Client
	logger *zap.Logger
}

// NewBlockQuery creates a new BlockQuery instance
func NewBlockQuery(client *Client, logger *zap.Logger) *BlockQuery {
	return &BlockQuery{
		client: client,
		logger: logger,
	}
}

// GetBlockByNumber retrieves block information by block number
func (bq *BlockQuery) GetBlockByNumber(ctx context.Context, blockNumber uint64) (*BlockInfo, error) {
	if !bq.client.IsConnected() {
		return nil, fmt.Errorf("client not connected")
	}

	bq.logger.Debug("Querying block by number", zap.Uint64("blockNumber", blockNumber))

	network := bq.client.GetNetwork()
	if network == nil {
		return nil, fmt.Errorf("network not available")
	}
	qscc := network.GetContract("qscc")

	blockNumBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(blockNumBytes, blockNumber)

	blockBytes, err := qscc.EvaluateTransaction("GetBlockByNumber", bq.client.config.ChannelName, string(blockNumBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to query block %d: %w", blockNumber, err)
	}

	return bq.parseBlock(blockBytes)
}

// GetBlockByTxID retrieves block containing a specific transaction
func (bq *BlockQuery) GetBlockByTxID(ctx context.Context, txID string) (*BlockInfo, error) {
	if !bq.client.IsConnected() {
		return nil, fmt.Errorf("client not connected")
	}

	bq.logger.Debug("Querying block by transaction ID", zap.String("txID", txID))

	network := bq.client.GetNetwork()
	if network == nil {
		return nil, fmt.Errorf("network not available")
	}
	qscc := network.GetContract("qscc")

	blockBytes, err := qscc.EvaluateTransaction("GetBlockByTransID", bq.client.config.ChannelName, txID)
	if err != nil {
		return nil, fmt.Errorf("failed to query block for tx %s: %w", txID, err)
	}

	return bq.parseBlock(blockBytes)
}

// GetTransactionByID retrieves detailed transaction information
func (bq *BlockQuery) GetTransactionByID(ctx context.Context, txID string) (*TransactionInfo, error) {
	if !bq.client.IsConnected() {
		return nil, fmt.Errorf("client not connected")
	}

	bq.logger.Debug("Querying transaction by ID", zap.String("txID", txID))

	network := bq.client.GetNetwork()
	if network == nil {
		return nil, fmt.Errorf("network not available")
	}
	qscc := network.GetContract("qscc")

	ptBytes, err := qscc.EvaluateTransaction("GetTransactionByID", bq.client.config.ChannelName, txID)
	if err != nil {
		return nil, fmt.Errorf("failed to query transaction %s: %w", txID, err)
	}

	return bq.parseProcessedTransaction(ptBytes, txID)
}

// GetTransactionValidationCode gets the validation code for a transaction
func (bq *BlockQuery) GetTransactionValidationCode(ctx context.Context, txID string) (int32, error) {
	txInfo, err := bq.GetTransactionByID(ctx, txID)
	if err != nil {
		return -1, err
	}
	return txInfo.ValidationCode, nil
}

// VerifyTransactionInBlock verifies that a transaction exists in a specific block
func (bq *BlockQuery) VerifyTransactionInBlock(ctx context.Context, txID string, blockNumber uint64) (bool, error) {
	if !bq.client.IsConnected() {
		return false, fmt.Errorf("client not connected")
	}

	blockInfo, err := bq.GetBlockByTxID(ctx, txID)
	if err != nil {
		return false, fmt.Errorf("failed to get block for tx: %w", err)
	}

	return blockInfo.BlockNumber == blockNumber, nil
}

// ExtractEndorsementsFromTx extracts all endorsement signatures from a transaction
func (bq *BlockQuery) ExtractEndorsementsFromTx(ctx context.Context, txID string) ([]*EndorsementInfo, error) {
	txInfo, err := bq.GetTransactionByID(ctx, txID)
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction: %w", err)
	}

	return txInfo.Endorsements, nil
}

// parseBlock parses raw block bytes into BlockInfo
func (bq *BlockQuery) parseBlock(blockBytes []byte) (*BlockInfo, error) {
	block := &common.Block{}
	if err := proto.Unmarshal(blockBytes, block); err != nil {
		return nil, fmt.Errorf("failed to unmarshal block: %w", err)
	}

	header := block.GetHeader()
	if header == nil {
		return nil, fmt.Errorf("block header is nil")
	}

	info := &BlockInfo{
		BlockNumber:  header.GetNumber(),
		PreviousHash: hex.EncodeToString(header.GetPreviousHash()),
		DataHash:     hex.EncodeToString(header.GetDataHash()),
		BlockHash:    ComputeBlockHeaderHash(header),
	}

	if block.GetData() != nil {
		info.TxCount = len(block.GetData().GetData())
	}

	return info, nil
}

// parseProcessedTransaction parses raw ProcessedTransaction bytes
func (bq *BlockQuery) parseProcessedTransaction(ptBytes []byte, txID string) (*TransactionInfo, error) {
	pt := &fabpeer.ProcessedTransaction{}
	if err := proto.Unmarshal(ptBytes, pt); err != nil {
		return nil, fmt.Errorf("failed to unmarshal processed transaction: %w", err)
	}

	txInfo := &TransactionInfo{
		TxID:           txID,
		ValidationCode: pt.GetValidationCode(),
	}

	envelope := pt.GetTransactionEnvelope()
	if envelope == nil {
		return txInfo, nil
	}

	// Parse envelope payload
	payload := &common.Payload{}
	if err := proto.Unmarshal(envelope.GetPayload(), payload); err != nil {
		return txInfo, nil
	}

	// Extract channel header info
	if payload.GetHeader() != nil && payload.GetHeader().GetChannelHeader() != nil {
		chHdr := &common.ChannelHeader{}
		if err := proto.Unmarshal(payload.GetHeader().GetChannelHeader(), chHdr); err == nil {
			txInfo.ChannelID = chHdr.GetChannelId()
			txInfo.TxID = chHdr.GetTxId()
			if chHdr.GetTimestamp() != nil {
				txInfo.Timestamp = chHdr.GetTimestamp().GetSeconds()*1000 + int64(chHdr.GetTimestamp().GetNanos()/1000000)
			}
		}
	}

	// Extract creator identity
	if payload.GetHeader() != nil && payload.GetHeader().GetSignatureHeader() != nil {
		sigHdr := &common.SignatureHeader{}
		if err := proto.Unmarshal(payload.GetHeader().GetSignatureHeader(), sigHdr); err == nil {
			sid := &msp.SerializedIdentity{}
			if err := proto.Unmarshal(sigHdr.GetCreator(), sid); err == nil {
				txInfo.CreatorMSPID = sid.GetMspid()
				txInfo.CreatorCert = sid.GetIdBytes()
			}
		}
	}

	// Parse Transaction to extract endorsements
	tx := &fabpeer.Transaction{}
	if err := proto.Unmarshal(payload.GetData(), tx); err != nil {
		return txInfo, nil
	}

	for _, action := range tx.GetActions() {
		ccPayload := &fabpeer.ChaincodeActionPayload{}
		if err := proto.Unmarshal(action.GetPayload(), ccPayload); err != nil {
			continue
		}

		endorsedAction := ccPayload.GetAction()
		if endorsedAction == nil {
			continue
		}

		for _, e := range endorsedAction.GetEndorsements() {
			sid := &msp.SerializedIdentity{}
			if err := proto.Unmarshal(e.GetEndorser(), sid); err != nil {
				continue
			}

			txInfo.Endorsements = append(txInfo.Endorsements, &EndorsementInfo{
				MSPID:       sid.GetMspid(),
				Certificate: sid.GetIdBytes(),
				Signature:   e.GetSignature(),
			})
		}
	}

	return txInfo, nil
}

// asn1BlockHeader mirrors Fabric's internal ASN.1 block-header structure.
// Fabric hashes block headers via ASN.1 DER encoding, NOT protobuf.
// See: fabric/protoutil/commonutils.go – BlockHeaderBytes().
type asn1BlockHeader struct {
	Number       *big.Int
	PreviousHash []byte
	DataHash     []byte
}

// ComputeBlockHeaderHash computes the SHA256 hash of a block header
// using ASN.1 DER encoding – exactly as Fabric does internally.
func ComputeBlockHeaderHash(header *common.BlockHeader) string {
	asn1Hdr := asn1BlockHeader{
		Number:       new(big.Int).SetUint64(header.GetNumber()),
		PreviousHash: header.GetPreviousHash(),
		DataHash:     header.GetDataHash(),
	}
	headerBytes, err := asn1.Marshal(asn1Hdr)
	if err != nil {
		panic(fmt.Sprintf("failed to ASN.1-marshal block header: %v", err))
	}
	hash := sha256.Sum256(headerBytes)
	return hex.EncodeToString(hash[:])
}

// ComputeCommitmentHash computes commitment hash for transaction data
func ComputeCommitmentHash(channelID, chaincodeID, function string, args [][]byte) string {
	h := sha256.New()
	h.Write([]byte(channelID))
	h.Write([]byte(chaincodeID))
	h.Write([]byte(function))
	for _, arg := range args {
		h.Write(arg)
	}
	return hex.EncodeToString(h.Sum(nil))
}
