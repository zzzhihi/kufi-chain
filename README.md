# Fabric Payment Gateway

A high-performance Go backend system for recording financial transactions on a Hyperledger Fabric consortium network. This system provides an immutable audit ledger for A → B X VND transactions with verifiable receipts.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Client Application                                 │
│                    (Internal Core Banking System)                           │
└─────────────────────────────────────────┬───────────────────────────────────┘
                                          │ mTLS
                                          ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Payment Gateway (Go)                                 │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐│
│  │  REST API   │  │  Worker     │  │  Receipt    │  │  Idempotency/       ││
│  │  Handlers   │  │  Pool       │  │  Builder    │  │  Nonce Store        ││
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘  └─────────────────────┘│
│         │                │                │                                  │
│  ┌──────┴────────────────┴────────────────┴─────────────────────────────┐  │
│  │                    Fabric Gateway SDK Client                          │  │
│  └──────────────────────────────────┬───────────────────────────────────┘  │
└─────────────────────────────────────┼───────────────────────────────────────┘
                                      │ gRPC/TLS
                    ┌─────────────────┼─────────────────┐
                    ▼                 ▼                 ▼
           ┌───────────────┐ ┌───────────────┐ ┌───────────────┐
           │  BankA Peer   │ │  BankB Peer   │ │ Regulator Peer│
           │  (Endorser)   │ │  (Endorser)   │ │  (Observer)   │
           └───────┬───────┘ └───────┬───────┘ └───────────────┘
                   │                 │
           ┌───────┴─────────────────┴───────┐
           │         Ordering Service         │
           │          (Raft Consensus)        │
           └──────────────────────────────────┘
```

## Key Features

### 1. Multi-Org Endorsement
- **Intra-bank transfers**: Single org endorsement
- **Inter-bank transfers**: Both sender and receiver banks must endorse
- **High-risk transactions**: Additional regulator endorsement required

### 2. Private Data Collections (PDC)
- On-chain: Only commitment hash + metadata
- PDC: Full transaction details (from/to/amount)
- Separate collections for different transaction types

### 3. Verifiable Receipts
Receipt contains all information to verify:
- Transaction committed to specific block
- Transaction validation status (VALID)
- Endorsement policy satisfied
- Cryptographic binding to original payload

### 4. Security Features
- mTLS for client authentication
- Idempotency keys (prevent duplicate submissions)
- Nonce + timestamp (replay protection)
- Audit logging

## Project Structure

```
chain/
├── cmd/
│   └── gateway/
│       └── main.go              # Application entry point
├── internal/
│   ├── api/
│   │   ├── handlers.go          # HTTP handlers
│   │   ├── models.go            # Request/Response DTOs
│   │   ├── middleware.go        # HTTP middleware
│   │   └── store.go             # In-memory stores
│   ├── config/
│   │   └── config.go            # Configuration management
│   ├── fabric/
│   │   ├── client.go            # Fabric Gateway client
│   │   ├── query.go             # Block/Transaction queries
│   │   └── endorsement.go       # Endorsement verification
│   └── receipt/
│       ├── receipt.go           # Receipt generation
│       └── verify.go            # Receipt verification
├── chaincode/
│   └── payment/
│       ├── go.mod
│       └── payment.go           # Smart contract
├── configs/
│   ├── config.yaml              # Gateway configuration
│   └── collections_config.yaml  # PDC definitions
└── README.md
```

## Receipt Schema (v1)

```json
{
  "schema_version": "v1",
  "channel_id": "payment-channel",
  "tx_id": "abc123...",
  "chaincode_id": "payment",
  "commitment_hash": "sha256-hash-of-transfer-details",
  "block_number": 12345,
  "block_hash": "block-header-hash",
  "tx_index": 0,
  "validation_code": 0,
  "validation_code_name": "VALID",
  "endorsements": [
    {
      "msp_id": "BankAMSP",
      "endorser_id": "peer0.banka.example.com",
      "cert_fingerprint": "sha256-of-cert",
      "signature_hex": "3045022100...",
      "signature_valid": true,
      "cert_chain_valid": true
    }
  ],
  "policy_id": "inter_bank_standard",
  "policy_met": true,
  "timestamps": {
    "client_submit": 1705123456789,
    "endorsement_time": 1705123456800,
    "block_commit": 1705123456900,
    "receipt_created": 1705123456950
  },
  "status_endpoint": "https://gateway.example.com/v1/receipt/status",
  "internal_ref": "TXN-2024-001",
  "receipt_hash": "sha256-of-receipt-content"
}
```

## Transaction Flow

```
1. Client submits transfer request
   POST /v1/transfer
   {
     "from_id": "ACC001",
     "to_id": "ACC002",
     "amount_vnd": 1000000,
     "internal_ref": "TXN-001",
     "idempotency_key": "unique-key-123",
     "nonce": "random-32-bytes",
     "timestamp": 1705123456789
   }

2. Gateway validates request
   - Check idempotency key
   - Validate nonce (replay protection)
   - Verify timestamp within window

3. Compute commitment hash
   SHA256(channel + chaincode + function + from + to + amount + ref + timestamp + nonce)

4. Submit to Fabric
   - Create proposal with transient data (PDC)
   - Collect endorsements from required orgs
   - Submit to orderer
   - Wait for commit event

5. Build verifiable receipt
   - Extract endorsements from transaction
   - Verify signatures and cert chains
   - Compute receipt hash

6. Return receipt to client
```

## API Endpoints

### POST /v1/transfer

Submit a new transfer transaction.

**Request:**
```json
{
  "from_id": "string",
  "to_id": "string",
  "amount_vnd": 1000000,
  "memo": "Payment for services",
  "internal_ref": "TXN-2024-001",
  "idempotency_key": "unique-key-123",
  "nonce": "random-string-min-16-chars",
  "timestamp": 1705123456789,
  "transfer_type": "inter_bank",
  "user_sig": "optional-client-signature"
}
```

**Response:**
```json
{
  "success": true,
  "tx_id": "abc123...",
  "receipt": { ... },
  "processed_at": 1705123456950
}
```

### GET /v1/receipt/{tx_id}

Retrieve a stored receipt by transaction ID.

### POST /v1/receipt/verify

Verify a receipt's validity.

**Request:**
```json
{
  "receipt": { ... },
  "expected_commitment_hash": "optional-hash-to-verify",
  "verify_on_chain": false
}
```

## Quick Start

### Prerequisites

- Go 1.21+
- Hyperledger Fabric 2.5+ test network
- Docker & Docker Compose

### 1. Start Fabric Test Network

```bash
# Clone fabric-samples
git clone https://github.com/hyperledger/fabric-samples.git
cd fabric-samples/test-network

# Start network with CA
./network.sh up createChannel -ca -c payment-channel

# Deploy chaincode
./network.sh deployCC -ccn payment -ccp /path/to/chaincode/payment -ccl go \
  -cccg /path/to/configs/collections_config.yaml
```

### 2. Configure Gateway

```bash
# Copy sample config
cp configs/config.yaml.example configs/config.yaml

# Edit configuration
# - Update MSP paths
# - Update peer endpoints
# - Configure TLS certificates
```

### 3. Build and Run

```bash
# Download dependencies
go mod download

# Build
go build -o gateway ./cmd/gateway

# Run
./gateway -config configs/config.yaml
```

### 4. Test with cURL

```bash
# Submit transfer
curl -X POST http://localhost:8080/v1/transfer \
  -H "Content-Type: application/json" \
  -d '{
    "from_id": "ACC001",
    "to_id": "ACC002",
    "amount_vnd": 1000000,
    "memo": "Test transfer",
    "internal_ref": "TXN-001",
    "idempotency_key": "test-key-001",
    "nonce": "random-nonce-1234567890",
    "timestamp": 1705123456789,
    "transfer_type": "intra_bank"
  }'

# Get receipt
curl http://localhost:8080/v1/receipt/{tx_id}

# Verify receipt
curl -X POST http://localhost:8080/v1/receipt/verify \
  -H "Content-Type: application/json" \
  -d '{"receipt": {...}}'

# Health check
curl http://localhost:8080/health
```

## Performance Optimization

### 1. Fabric Network
- **Block Size**: Configure `BatchSize.MaxMessageCount` (default: 500)
- **Batch Timeout**: Configure `BatchTimeout` (default: 2s)
- **State DB**: LevelDB for simple key queries, CouchDB for rich queries

### 2. Gateway
- **Worker Pool**: 50 concurrent workers (configurable)
- **Connection Pooling**: gRPC connection reuse
- **Async Submit**: Non-blocking transaction submission with event listening
- **Timeout Handling**: Configurable timeouts for each operation

### 3. Retry Logic
- Exponential backoff: 100ms → 200ms → 400ms → ... → 5s max
- Max 3 retry attempts
- Retry on transient failures only

## Endorsement Policies

| Transaction Type | Policy | Required Endorsers |
|-----------------|--------|-------------------|
| Intra-bank (standard) | OR | BankA OR BankB |
| Inter-bank (standard) | AND | BankA AND BankB |
| High-risk (> 1B VND) | AND | BankA AND BankB AND Regulator |

## Security Considerations

1. **mTLS**: All client connections require valid certificates
2. **Idempotency**: 24-hour TTL prevents duplicate submissions
3. **Replay Protection**: Nonce + 5-minute timestamp window
4. **PII Protection**: Sensitive data only in PDC, not on public ledger
5. **Audit Trail**: All requests logged with hashed bodies

## Monitoring

### Health Check
```bash
GET /health
{
  "status": "ok",
  "fabric_status": "connected",
  "version": "1.0.0",
  "timestamp": "2024-01-13T12:00:00Z"
}
```

### Metrics (TODO)
- Prometheus metrics endpoint
- Transaction latency histograms
- Endorsement failure rates
- Worker pool utilization

## Testing

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run specific package tests
go test ./internal/receipt/...
```

## License

This project is licensed under the Apache License 2.0.

## Contributing

1. Fork the repository
2. Create a feature branch
3. Commit changes
4. Push to the branch
5. Create a Pull Request
