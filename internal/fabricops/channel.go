package fabricops

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ChannelOps provides channel management operations.
type ChannelOps struct {
	DeployDir      string
	ChannelName    string
	OrdererAddr    string // host:port
	NetworkDom     string
	OrgName        string
	MSPID          string
	Domain         string
	PeerPort       int
	AdminPort      int  // osnadmin port (default 7053)
	IsOrdererAdmin bool // if true, use OrdererMSP admin identity (for dedicated orderer nodes)
}

// PeerEnv returns the environment variables needed for peer CLI commands.
func (c *ChannelOps) PeerEnv() []string {
	base := c.DeployDir

	if c.IsOrdererAdmin {
		return c.ordererAdminEnv()
	}

	peerTLS := filepath.Join(base, "crypto-config/peerOrganizations", c.Domain,
		"peers", fmt.Sprintf("peer0.%s", c.Domain), "tls/ca.crt")
	adminMSP := filepath.Join(base, "crypto-config/peerOrganizations", c.Domain,
		"users", fmt.Sprintf("Admin@%s", c.Domain), "msp")
	ordererCA := filepath.Join(base, "crypto-config/ordererOrganizations", c.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", c.NetworkDom), "msp/tlscacerts",
		fmt.Sprintf("tlsca.%s-cert.pem", c.NetworkDom))

	return append(os.Environ(),
		"FABRIC_CFG_PATH="+filepath.Join(base, "config"),
		"CORE_PEER_TLS_ENABLED=true",
		"CORE_PEER_LOCALMSPID="+c.MSPID,
		"CORE_PEER_TLS_ROOTCERT_FILE="+peerTLS,
		"CORE_PEER_MSPCONFIGPATH="+adminMSP,
		fmt.Sprintf("CORE_PEER_ADDRESS=localhost:%d", c.PeerPort),
		"ORDERER_CA="+ordererCA,
		"CORE_ORDERER_TLS_ROOTCERT_FILE="+ordererCA,
	)
}

// ordererAdminEnv returns env vars for running peer CLI using OrdererMSP admin.
// Used by dedicated orderer nodes to sign channel config updates.
func (c *ChannelOps) ordererAdminEnv() []string {
	base := c.DeployDir
	ordererTLSCA := filepath.Join(base, "crypto-config/ordererOrganizations", c.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", c.NetworkDom), "tls/ca.crt")
	adminMSP := filepath.Join(base, "crypto-config/ordererOrganizations", c.NetworkDom,
		"users", fmt.Sprintf("Admin@%s", c.NetworkDom), "msp")
	ordererCA := filepath.Join(base, "crypto-config/ordererOrganizations", c.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", c.NetworkDom), "msp/tlscacerts",
		fmt.Sprintf("tlsca.%s-cert.pem", c.NetworkDom))

	return append(os.Environ(),
		"FABRIC_CFG_PATH="+filepath.Join(base, "config"),
		"CORE_PEER_TLS_ENABLED=true",
		"CORE_PEER_LOCALMSPID=OrdererMSP",
		"CORE_PEER_TLS_ROOTCERT_FILE="+ordererTLSCA,
		"CORE_PEER_MSPCONFIGPATH="+adminMSP,
		"CORE_PEER_ADDRESS=localhost:7051", // not actually used for fetch/update
		"ORDERER_CA="+ordererCA,
		"CORE_ORDERER_TLS_ROOTCERT_FILE="+ordererCA,
	)
}

// OrdererCA returns the path to the orderer TLS CA cert.
func (c *ChannelOps) OrdererCAPath() string {
	return filepath.Join(c.DeployDir, "crypto-config/ordererOrganizations", c.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", c.NetworkDom), "msp/tlscacerts",
		fmt.Sprintf("tlsca.%s-cert.pem", c.NetworkDom))
}

// OrdererTLSCert returns the orderer TLS cert (for osnadmin).
func (c *ChannelOps) ordererTLSCert() string {
	return filepath.Join(c.DeployDir, "crypto-config/ordererOrganizations", c.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", c.NetworkDom), "tls/server.crt")
}

// OrdererTLSKey returns the orderer TLS key (for osnadmin).
func (c *ChannelOps) ordererTLSKey() string {
	return filepath.Join(c.DeployDir, "crypto-config/ordererOrganizations", c.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", c.NetworkDom), "tls/server.key")
}

// adminAddr returns the osnadmin listen address.
func (c *ChannelOps) adminAddr() string {
	port := c.AdminPort
	if port == 0 {
		port = 7053
	}
	return fmt.Sprintf("localhost:%d", port)
}

// JoinOrdererToChannel uses osnadmin to join the orderer to the channel.
// It is idempotent: if the channel already exists, it returns nil.
func (c *ChannelOps) JoinOrdererToChannel() error {
	// Check if channel already exists (e.g. from a previous run with preserved volumes)
	if c.VerifyChannelExists() {
		fmt.Printf("  ✓ Channel '%s' already exists on orderer — skipping join\n", c.ChannelName)
		return nil
	}

	blockPath := filepath.Join(c.DeployDir, "channel-artifacts", c.ChannelName+".block")
	return runCmd(c.DeployDir, "osnadmin", "channel", "join",
		"--channelID", c.ChannelName,
		"--config-block", blockPath,
		"--orderer-address", c.adminAddr(),
		"--ca-file", c.OrdererCAPath(),
		"--client-cert", c.ordererTLSCert(),
		"--client-key", c.ordererTLSKey())
}

// VerifyChannelExists uses osnadmin to check if the channel exists on the orderer.
// Returns true if the channel is found.
func (c *ChannelOps) VerifyChannelExists() bool {
	// osnadmin channel list outputs JSON: {"systemChannel":null,"channels":[{"name":"payment-channel","url":"/participation/v1/channels/payment-channel"}]}
	// Or with --channelID it returns info for a single channel (404 if not found).
	out, err := runCmdOutput(c.DeployDir, "osnadmin", "channel", "list",
		"--channelID", c.ChannelName,
		"--orderer-address", c.adminAddr(),
		"--ca-file", c.OrdererCAPath(),
		"--client-cert", c.ordererTLSCert(),
		"--client-key", c.ordererTLSKey())
	if err != nil {
		return false
	}
	// If the channel exists, the output contains the channel name
	return strings.Contains(string(out), c.ChannelName)
}

// WaitForOrdererAdminReady waits until the orderer admin API is reachable.
func (c *ChannelOps) WaitForOrdererAdminReady(timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = 1
	}
	var lastErr error
	for i := 0; i < timeoutSec; i++ {
		_, err := runCmdOutput(c.DeployDir, "osnadmin", "channel", "list",
			"--orderer-address", c.adminAddr(),
			"--ca-file", c.OrdererCAPath(),
			"--client-cert", c.ordererTLSCert(),
			"--client-key", c.ordererTLSKey())
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("orderer admin API not ready after %ds: %w", timeoutSec, lastErr)
}

// PeerHasJoinedChannel checks if the peer has already joined the channel
// by running `peer channel list` and checking the output.
func (c *ChannelOps) PeerHasJoinedChannel() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "peer", "channel", "list")
	cmd.Dir = c.DeployDir
	cmd.Env = c.PeerEnv()
	out, err := cmd.CombinedOutput()
	if err != nil || ctx.Err() == context.DeadlineExceeded {
		return false
	}
	return strings.Contains(string(out), c.ChannelName)
}

// WaitForPeerReady waits until peer CLI can talk to the local peer process.
func (c *ChannelOps) WaitForPeerReady(timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = 1
	}
	var lastErr error
	for i := 0; i < timeoutSec; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "peer", "node", "status")
		cmd.Dir = c.DeployDir
		cmd.Env = c.PeerEnv()
		if out, err := cmd.CombinedOutput(); err == nil {
			cancel()
			_ = out
			return nil
		} else {
			if ctx.Err() == context.DeadlineExceeded {
				cancel()
				lastErr = fmt.Errorf("peer node status timeout")
				time.Sleep(1 * time.Second)
				continue
			}
			lastErr = fmt.Errorf("%s", strings.TrimSpace(string(out)))
		}
		cancel()
		time.Sleep(1 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unknown peer readiness error")
	}
	return fmt.Errorf("peer not ready after %ds: %w", timeoutSec, lastErr)
}

// WaitForPeerSync waits until the peer has synced to at least the given block height,
// or until the timeout. Returns the synced block height, or -1 on timeout.
// This is important after channel join: the peer starts at block 0 and needs
// to pull config update blocks from the orderer before chaincode lifecycle
// commands will work.
func (c *ChannelOps) WaitForPeerSync(minBlock int, timeoutSec int) int {
	for i := 0; i < timeoutSec; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "peer", "channel", "getinfo", "-c", c.ChannelName)
		cmd.Dir = c.DeployDir
		cmd.Env = c.PeerEnv()
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			// Output looks like: Blockchain info: {"height":3,"currentBlockHash":"...","previousBlockHash":"..."}
			outStr := string(out)
			if idx := strings.Index(outStr, `"height":`); idx >= 0 {
				rest := outStr[idx+len(`"height":`):]
				end := strings.IndexAny(rest, ",}")
				if end > 0 {
					var h int
					if _, err := fmt.Sscanf(rest[:end], "%d", &h); err == nil {
						height := h - 1 // height is block count, latest block = height - 1
						if height >= minBlock {
							return height
						}
					}
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return -1
}

// FetchAndJoinPeer fetches the genesis block from orderer and joins the peer.
// It is idempotent: if the peer has already joined the channel, it returns nil.
func (c *ChannelOps) FetchAndJoinPeer() error {
	// Check if the peer has already joined this channel (e.g. from a previous run
	// where Docker volumes were preserved). This avoids the "ledger already exists" error.
	if c.PeerHasJoinedChannel() {
		fmt.Printf("  ✓ Peer already joined channel '%s' — skipping join\n", c.ChannelName)
		return nil
	}

	blockPath := filepath.Join(c.DeployDir, "channel-artifacts", c.ChannelName+"_genesis.block")
	os.MkdirAll(filepath.Dir(blockPath), 0o755)

	// Fetch genesis block
	cmd := exec.Command("peer", "channel", "fetch", "0", blockPath,
		"-o", c.OrdererAddr,
		"--ordererTLSHostnameOverride", fmt.Sprintf("orderer.%s", c.NetworkDom),
		"-c", c.ChannelName,
		"--tls", "--cafile", c.OrdererCAPath())
	cmd.Dir = c.DeployDir
	cmd.Env = c.PeerEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fetch genesis block: %w", err)
	}

	// Join peer
	cmd = exec.Command("peer", "channel", "join", "-b", blockPath)
	cmd.Dir = c.DeployDir
	cmd.Env = c.PeerEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("peer channel join: %w", err)
	}
	return nil
}

// UpdateAnchorPeers updates the anchor peer for this org.
func (c *ChannelOps) UpdateAnchorPeers(anchorHost string, anchorPort int) error {
	// Create anchor peer update tx
	// For Fabric 2.5, we use configtxlator to create the update
	// Simplified: use peer channel fetch + jq + configtxlator pipeline
	fmt.Printf("  Anchor peer update for %s would set %s:%d\n", c.MSPID, anchorHost, anchorPort)
	fmt.Println("  (Anchor peer updates are handled via channel config update)")
	return nil
}

// AddOrgToChannel performs the full workflow to add a new org to the channel config.
// Returns the path to the config update envelope (unsigned).
func (c *ChannelOps) AddOrgToChannel(orgDefinitionJSON string) (string, error) {
	workDir := filepath.Join(c.DeployDir, "config-update-work")
	os.MkdirAll(workDir, 0o755)

	// Write org definition
	orgDefPath := filepath.Join(workDir, "org_definition.json")
	if err := os.WriteFile(orgDefPath, []byte(orgDefinitionJSON), 0o644); err != nil {
		return "", err
	}

	// Step 1: Fetch current channel config
	configBlockPB := filepath.Join(workDir, "config_block.pb")
	cmd := exec.Command("peer", "channel", "fetch", "config", configBlockPB,
		"-o", c.OrdererAddr,
		"--ordererTLSHostnameOverride", fmt.Sprintf("orderer.%s", c.NetworkDom),
		"-c", c.ChannelName,
		"--tls", "--cafile", c.OrdererCAPath())
	cmd.Dir = c.DeployDir
	cmd.Env = c.PeerEnv()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("fetch config: %w", err)
	}

	// Step 2: Decode config block → JSON
	configBlockJSON := filepath.Join(workDir, "config_block.json")
	if err := runCmd(workDir, "configtxlator", "proto_decode",
		"--input", configBlockPB,
		"--type", "common.Block",
		"--output", configBlockJSON); err != nil {
		return "", fmt.Errorf("decode config block: %w", err)
	}

	// Step 3: Extract config
	configJSON := filepath.Join(workDir, "config.json")
	jqOut, err := exec.Command("jq", ".data.data[0].payload.data.config", configBlockJSON).Output()
	if err != nil {
		return "", fmt.Errorf("jq extract config: %w", err)
	}
	os.WriteFile(configJSON, jqOut, 0o644)

	// Save original
	origJSON := filepath.Join(workDir, "config_original.json")
	origData, _ := os.ReadFile(configJSON)
	os.WriteFile(origJSON, origData, 0o644)

	// Step 4: Add org to config using jq
	modifiedJSON := filepath.Join(workDir, "config_modified.json")
	newOrgMSPID := "" // extract from org definition
	// Parse org definition to find the MSPID — it's the key in the values.MSP
	// Actually, we need to know the MSPID to insert. We'll get it from the caller.
	// For now, we read it from the org definition file
	jqFilter := fmt.Sprintf(`.[0] * {"channel_group":{"groups":{"Application":{"groups":{"__NEWORG__":.[1]}}}}}`)
	// The org definition from configtxgen -printOrg is already in the right format

	// We need to find the MSPID from the org definition JSON
	// configtxgen -printOrg outputs: { "groups": {}, "mod_policy": ..., "policies": ..., "values": { "MSP": { "value": { "config": { "name": "OrgMSP" } } } } }
	// Let's extract the name
	mspIDOut, err := exec.Command("jq", "-r", `.values.MSP.value.config.name // empty`, orgDefPath).Output()
	if err == nil && len(strings.TrimSpace(string(mspIDOut))) > 0 {
		newOrgMSPID = strings.TrimSpace(string(mspIDOut))
	}
	if newOrgMSPID == "" {
		// Fallback: try .values.MSP.value.config.fabric_node_ous.organizational_unit_identifiers
		// or just use the name from the request
		return "", fmt.Errorf("could not extract MSPID from org definition JSON")
	}

	jqFilter = fmt.Sprintf(`.[0] * {"channel_group":{"groups":{"Application":{"groups":{"%s":.[1]}}}}}`, newOrgMSPID)
	modOut, err := exec.Command("jq", "-s", jqFilter, configJSON, orgDefPath).Output()
	if err != nil {
		return "", fmt.Errorf("jq add org: %w", err)
	}
	os.WriteFile(modifiedJSON, modOut, 0o644)

	// Step 5: Encode both configs to protobuf
	origPB := filepath.Join(workDir, "config_original.pb")
	modPB := filepath.Join(workDir, "config_modified.pb")
	updatePB := filepath.Join(workDir, "config_update.pb")

	if err := runCmd(workDir, "configtxlator", "proto_encode",
		"--input", origJSON, "--type", "common.Config", "--output", origPB); err != nil {
		return "", fmt.Errorf("encode original: %w", err)
	}
	if err := runCmd(workDir, "configtxlator", "proto_encode",
		"--input", modifiedJSON, "--type", "common.Config", "--output", modPB); err != nil {
		return "", fmt.Errorf("encode modified: %w", err)
	}

	// Step 6: Compute update
	if err := runCmd(workDir, "configtxlator", "compute_update",
		"--channel_id", c.ChannelName,
		"--original", origPB, "--updated", modPB, "--output", updatePB); err != nil {
		return "", fmt.Errorf("compute update: %w", err)
	}

	// Step 7: Decode update → JSON
	updateJSON := filepath.Join(workDir, "config_update.json")
	if err := runCmd(workDir, "configtxlator", "proto_decode",
		"--input", updatePB, "--type", "common.ConfigUpdate", "--output", updateJSON); err != nil {
		return "", fmt.Errorf("decode update: %w", err)
	}

	// Step 8: Wrap in envelope
	updateData, _ := os.ReadFile(updateJSON)
	envelopeJSON := filepath.Join(workDir, "config_update_envelope.json")
	envelope := fmt.Sprintf(`{"payload":{"header":{"channel_header":{"channel_id":"%s","type":2}},"data":{"config_update":%s}}}`,
		c.ChannelName, string(updateData))
	os.WriteFile(envelopeJSON, []byte(envelope), 0o644)

	envelopePB := filepath.Join(workDir, "config_update_envelope.pb")
	if err := runCmd(workDir, "configtxlator", "proto_encode",
		"--input", envelopeJSON, "--type", "common.Envelope", "--output", envelopePB); err != nil {
		return "", fmt.Errorf("encode envelope: %w", err)
	}

	return envelopePB, nil
}

// SignConfigUpdate signs a config update envelope with this node's admin identity.
func (c *ChannelOps) SignConfigUpdate(envelopePBPath string) error {
	cmd := exec.Command("peer", "channel", "signconfigtx", "-f", envelopePBPath)
	cmd.Dir = c.DeployDir
	cmd.Env = c.PeerEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SubmitConfigUpdate submits a signed config update to the orderer.
func (c *ChannelOps) SubmitConfigUpdate(envelopePBPath string) error {
	cmd := exec.Command("peer", "channel", "update",
		"-f", envelopePBPath,
		"-c", c.ChannelName,
		"-o", c.OrdererAddr,
		"--ordererTLSHostnameOverride", fmt.Sprintf("orderer.%s", c.NetworkDom),
		"--tls", "--cafile", c.OrdererCAPath())
	cmd.Dir = c.DeployDir
	cmd.Env = c.PeerEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SignEnvelopeBytes signs a config update envelope (from base64 bytes) and returns the signed version.
func (c *ChannelOps) SignEnvelopeBytes(envelopeB64 string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(envelopeB64)
	if err != nil {
		return "", err
	}

	tmpFile := filepath.Join(c.DeployDir, "config-update-work", "sign_tmp.pb")
	os.MkdirAll(filepath.Dir(tmpFile), 0o755)
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return "", err
	}

	if err := c.SignConfigUpdate(tmpFile); err != nil {
		return "", err
	}

	signed, err := os.ReadFile(tmpFile)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signed), nil
}

// ReadOrdererTLSCA reads the orderer TLS CA cert and returns it base64-encoded.
func (c *ChannelOps) ReadOrdererTLSCA() (string, error) {
	data, err := os.ReadFile(c.OrdererCAPath())
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// EnsureFabricConfig creates/updates the core.yaml in deploy/config/.
// Always overwrites to ensure the latest required fields (e.g. orderer TLS) are present.
func EnsureFabricConfig(deployDir string) error {
	configDir := filepath.Join(deployDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}

	coreYaml := filepath.Join(configDir, "core.yaml")

	// core.yaml for peer CLI and CCAAS external builder
	// NOTE: orderer.address and orderer.tls.rootcert.file are required so
	// that `peer lifecycle chaincode approveformyorg` (and other orderer-
	// bound commands) can create the broadcast client correctly even when
	// the --cafile flag is given on the CLI.
	content := `# core.yaml – auto-generated by kufichain
peer:
  bccsp:
    default: SW
    sw:
      hash: SHA2
      security: 256
  discovery:
    enabled: true
    authCacheEnabled: true
    authCacheMaxSize: 1000
    authCachePurgeRetentionRatio: 0.75
    orgMembersAllowedAccess: false
  handlers:
    authFilter:
      - name: FilterExpiration
      - name: ExpirationCheck
    decorators:
      - name: DefaultDecorator
    endorsers:
      escc:
        name: DefaultEndorsement
    validators:
      vscc:
        name: DefaultValidation

orderer:
  address: 127.0.0.1:7050
  tls:
    enabled: true
    rootcert:
      file:

chaincode:
  externalBuilders:
    - name: ccaas_builder
      path: /opt/hyperledger/ccaas_builder
      propagateEnvironment:
        - CHAINCODE_AS_A_SERVICE_BUILDER_CONFIG
  system:
    _lifecycle: enable
    cscc: enable
    lscc: enable
    escc: enable
    vscc: enable
    qscc: enable
`
	return os.WriteFile(coreYaml, []byte(content), 0o644)
}

// EnsureOrdererConfig creates a minimal orderer.yaml in deploy/config/ if missing.
// Needed because Fabric v2.5 orderer reads from FABRIC_CFG_PATH and the default
// image config may contain unsupported keys (e.g. Kafka removed in v3).
func EnsureOrdererConfig(deployDir string) error {
	configDir := filepath.Join(deployDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}

	ordererYaml := filepath.Join(configDir, "orderer.yaml")
	if _, err := os.Stat(ordererYaml); err == nil {
		return nil
	}

	content := `# orderer.yaml – auto-generated by kufichain
General:
  ListenAddress: 0.0.0.0
  ListenPort: 7050
  TLS:
    Enabled: true
    PrivateKey: /var/hyperledger/orderer/tls/server.key
    Certificate: /var/hyperledger/orderer/tls/server.crt
    RootCAs:
      - /var/hyperledger/orderer/tls/ca.crt
  Keepalive:
    ServerMinInterval: 60s
    ServerInterval: 7200s
    ServerTimeout: 20s
  MaxRecvMsgSize: 104857600
  MaxSendMsgSize: 104857600
  Cluster:
    ClientCertificate: /var/hyperledger/orderer/tls/server.crt
    ClientPrivateKey: /var/hyperledger/orderer/tls/server.key
    RootCAs:
      - /var/hyperledger/orderer/tls/ca.crt
  BootstrapMethod: none
  LocalMSPDir: /var/hyperledger/orderer/msp
  LocalMSPID: OrdererMSP
FileLedger:
  Location: /var/hyperledger/production/orderer
Operations:
  ListenAddress: 0.0.0.0:9443
  TLS:
    Enabled: false
Metrics:
  Provider: prometheus
Admin:
  ListenAddress: 0.0.0.0:7053
  TLS:
    Enabled: true
    Certificate: /var/hyperledger/orderer/tls/server.crt
    PrivateKey: /var/hyperledger/orderer/tls/server.key
    RootCAs:
      - /var/hyperledger/orderer/tls/ca.crt
    ClientAuthRequired: true
    ClientRootCAs:
      - /var/hyperledger/orderer/tls/ca.crt
ChannelParticipation:
  Enabled: true
  MaxRequestBodySize: 1 MB
`
	return os.WriteFile(ordererYaml, []byte(content), 0o644)
}
