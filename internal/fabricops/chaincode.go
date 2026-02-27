// Package fabricops — chaincode lifecycle management for CCAAS (Chaincode-as-a-Service).
//
// Handles: Docker image build, CCAAS packaging, install, approve, commit,
// and chaincode container lifecycle — all automated so stakeholders only
// need `kufichain run`.
package fabricops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ────────────────────────────────────────────────────────────────────
// ChaincodeOps — high-level chaincode lifecycle helper
// ────────────────────────────────────────────────────────────────────

// PeerEndpoint describes a peer for commit endorsement collection.
type PeerEndpoint struct {
	Addr        string // host:port reachable from CLI (e.g. "localhost:7051")
	TLSCertPath string // absolute path to the peer's TLS CA cert
	MgmtAddr    string // management API address (e.g. "http://localhost:9501")
}

// ChaincodeOps provides chaincode lifecycle operations for a single peer.
type ChaincodeOps struct {
	DeployDir     string // deploy dir with crypto-config/ and config/
	ChannelName   string
	OrdererAddr   string // host:port
	NetworkDom    string
	OrgName       string
	MSPID         string
	Domain        string
	PeerPort      int
	ChaincodeName string // e.g. "payment"
	ChaincodeVer  string // e.g. "1.0"
	CCLabel       string // e.g. "payment_1.0"
	ProjectRoot   string // absolute path to the chain/ project root
	// PeerEndpoints lists all known peer endpoints for commit endorsement.
	PeerEndpoints []PeerEndpoint
	// SignaturePolicy for the chaincode endorsement policy, e.g.
	// "OR('TechcombankMSP.peer','VietcombankMSP.peer')"
	// Leave empty to use the channel default.
	SignaturePolicy string
}

// peerEnv returns the environment variables needed for peer CLI commands.
func (cc *ChaincodeOps) peerEnv() []string {
	base := cc.DeployDir

	peerTLS := filepath.Join(base, "crypto-config/peerOrganizations", cc.Domain,
		"peers", fmt.Sprintf("peer0.%s", cc.Domain), "tls/ca.crt")
	adminMSP := filepath.Join(base, "crypto-config/peerOrganizations", cc.Domain,
		"users", fmt.Sprintf("Admin@%s", cc.Domain), "msp")
	ordererCA := filepath.Join(base, "crypto-config/ordererOrganizations", cc.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", cc.NetworkDom), "msp/tlscacerts",
		fmt.Sprintf("tlsca.%s-cert.pem", cc.NetworkDom))

	return append(os.Environ(),
		"FABRIC_CFG_PATH="+filepath.Join(base, "config"),
		"CORE_PEER_TLS_ENABLED=true",
		"CORE_PEER_LOCALMSPID="+cc.MSPID,
		"CORE_PEER_TLS_ROOTCERT_FILE="+peerTLS,
		"CORE_PEER_MSPCONFIGPATH="+adminMSP,
		fmt.Sprintf("CORE_PEER_ADDRESS=localhost:%d", cc.PeerPort),
		"ORDERER_CA="+ordererCA,
		"CORE_ORDERER_TLS_ROOTCERT_FILE="+ordererCA,
	)
}

func (cc *ChaincodeOps) ordererCAPath() string {
	return filepath.Join(cc.DeployDir, "crypto-config/ordererOrganizations", cc.NetworkDom,
		"orderers", fmt.Sprintf("orderer.%s", cc.NetworkDom), "msp/tlscacerts",
		fmt.Sprintf("tlsca.%s-cert.pem", cc.NetworkDom))
}

// ────────────────────────────────────────────────────────────────────
// 1. Docker image build
// ────────────────────────────────────────────────────────────────────

// ChaincodeImageTag returns the Docker image tag: "payment-chaincode:1.0"
func (cc *ChaincodeOps) ChaincodeImageTag() string {
	return fmt.Sprintf("%s-chaincode:%s", cc.ChaincodeName, cc.ChaincodeVer)
}

// BuildChaincodeImage builds the chaincode Docker image if not already present.
func (cc *ChaincodeOps) BuildChaincodeImage() error {
	tag := cc.ChaincodeImageTag()

	// Check if image already exists
	out, _ := exec.Command("docker", "images", "-q", tag).Output()
	if strings.TrimSpace(string(out)) != "" {
		return nil // image already exists
	}

	ccDir := filepath.Join(cc.ProjectRoot, "chaincode", cc.ChaincodeName)
	if _, err := os.Stat(filepath.Join(ccDir, "Dockerfile")); err != nil {
		return fmt.Errorf("no Dockerfile in %s: %w", ccDir, err)
	}

	cmd := exec.Command("docker", "build", "-t", tag, ".")
	cmd.Dir = ccDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build chaincode: %w", err)
	}
	return nil
}

// ────────────────────────────────────────────────────────────────────
// 2. CCAAS package creation
// ────────────────────────────────────────────────────────────────────

// ccContainerName returns the Docker container name for this org's chaincode.
// e.g. "payment-cc-techcombank"
func (cc *ChaincodeOps) ccContainerName() string {
	orgLower := toLower(cc.OrgName)
	return fmt.Sprintf("%s-cc-%s", cc.ChaincodeName, orgLower)
}

// BuildCCAASPackage creates a Fabric CCAAS chaincode package (.tar.gz) that
// points to the external chaincode container. Returns the path to the package.
// Always regenerates to ensure the connection address is up-to-date.
func (cc *ChaincodeOps) BuildCCAASPackage() (string, error) {
	pkgPath := filepath.Join(cc.DeployDir, fmt.Sprintf("%s-ccaas.tar.gz", cc.ChaincodeName))

	// Always regenerate to ensure correct connection address
	os.Remove(pkgPath)

	// connection.json — the peer connects to the chaincode at this address
	connJSON := fmt.Sprintf(`{
  "address": "%s:9999",
  "dial_timeout": "10s",
  "tls_required": false
}`, cc.ccContainerName())

	metaJSON := fmt.Sprintf(`{
  "type": "ccaas",
  "label": "%s"
}`, cc.CCLabel)

	// Build the nested tar: code.tar.gz contains connection.json
	var codeBuf bytes.Buffer
	if err := writeTarGz(&codeBuf, map[string][]byte{
		"connection.json": []byte(connJSON),
	}); err != nil {
		return "", fmt.Errorf("create code.tar.gz: %w", err)
	}

	// Build outer tar: contains code.tar.gz + metadata.json
	var outerBuf bytes.Buffer
	if err := writeTarGz(&outerBuf, map[string][]byte{
		"code.tar.gz":   codeBuf.Bytes(),
		"metadata.json": []byte(metaJSON),
	}); err != nil {
		return "", fmt.Errorf("create package tarball: %w", err)
	}

	if err := os.WriteFile(pkgPath, outerBuf.Bytes(), 0o644); err != nil {
		return "", fmt.Errorf("write package: %w", err)
	}
	return pkgPath, nil
}

// writeTarGz creates a tar.gz archive in-memory from a map of name→content.
func writeTarGz(buf *bytes.Buffer, files map[string][]byte) error {
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)
	for name, data := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}

// ────────────────────────────────────────────────────────────────────
// 3. Install chaincode
// ────────────────────────────────────────────────────────────────────

// InstalledPackageID returns the package ID if the chaincode label is already
// installed on this peer. Returns "" if not installed.
func (cc *ChaincodeOps) InstalledPackageID() string {
	cmd := exec.Command("peer", "lifecycle", "chaincode", "queryinstalled", "--output", "json")
	cmd.Dir = cc.DeployDir
	cmd.Env = cc.peerEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	var result struct {
		InstalledChaincodes []struct {
			PackageID string `json:"package_id"`
			Label     string `json:"label"`
		} `json:"installed_chaincodes"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return ""
	}
	for _, ic := range result.InstalledChaincodes {
		if ic.Label == cc.CCLabel {
			return ic.PackageID
		}
	}
	return ""
}

// InstallChaincode installs the CCAAS package on the peer.
// Returns the package ID.
func (cc *ChaincodeOps) InstallChaincode(pkgPath string) (string, error) {
	// Check if already installed
	if pkgID := cc.InstalledPackageID(); pkgID != "" {
		return pkgID, nil
	}

	cmd := exec.Command("peer", "lifecycle", "chaincode", "install", pkgPath)
	cmd.Dir = cc.DeployDir
	cmd.Env = cc.peerEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("install chaincode: %s — %w", string(out), err)
	}

	// Extract package ID from output or query again
	pkgID := cc.InstalledPackageID()
	if pkgID == "" {
		return "", fmt.Errorf("chaincode installed but could not find package ID")
	}
	return pkgID, nil
}

// ────────────────────────────────────────────────────────────────────
// 4. Approve chaincode for this org
// ────────────────────────────────────────────────────────────────────

// IsApprovedForMyOrg checks if chaincode is already approved for this org
// with the CURRENT signature policy. Uses checkcommitreadiness to ensure
// the approval matches the desired policy (not just any approval at the sequence).
func (cc *ChaincodeOps) IsApprovedForMyOrg(sequence int) bool {
	approvals := cc.CheckCommitReadiness(sequence)
	if approvals == nil {
		return false
	}
	return approvals[cc.MSPID]
}

func isLifecycleSyncingError(msg string) bool {
	lower := strings.ToLower(msg)
	patterns := []string{
		"creator org unknown",
		"creator is malformed",
		"failed to deserialize creator",
		"access denied",
		"channel does not exist",
		"not found in channel config",
		"connection refused",
		"context deadline exceeded",
		"service unavailable",
		"rpc error",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func (cc *ChaincodeOps) readinessArgs(sequence int) []string {
	args := []string{"lifecycle", "chaincode", "checkcommitreadiness",
		"-C", cc.ChannelName,
		"-n", cc.ChaincodeName,
		"--version", cc.ChaincodeVer,
		"--sequence", fmt.Sprintf("%d", sequence),
		"--output", "json"}
	if cc.SignaturePolicy != "" {
		args = append(args, "--signature-policy", cc.SignaturePolicy)
	}
	return args
}

// WaitForLifecycleReady waits until peer lifecycle commands can read channel config
// with this org identity. This avoids early approve/commit failures right after join.
func (cc *ChaincodeOps) WaitForLifecycleReady(sequence int, timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = 1
	}
	var lastErr error
	for i := 0; i < timeoutSec; i++ {
		cmd := exec.Command("peer", cc.readinessArgs(sequence)...)
		cmd.Dir = cc.DeployDir
		cmd.Env = cc.peerEnv()
		if out, err := cmd.CombinedOutput(); err == nil {
			_ = out
			return nil
		} else {
			msg := strings.TrimSpace(string(out))
			lastErr = fmt.Errorf("%s", msg)
			if !isLifecycleSyncingError(msg) {
				return fmt.Errorf("lifecycle readiness check failed: %w", lastErr)
			}
		}
		time.Sleep(1 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("unknown lifecycle readiness error")
	}
	return fmt.Errorf("lifecycle not ready after %ds: %w", timeoutSec, lastErr)
}

// ApproveChaincode approves the chaincode definition for this org.
// It retries if the peer hasn't synced the channel config yet (common after join).
func (cc *ChaincodeOps) ApproveChaincode(packageID string, sequence int) error {
	if cc.IsApprovedForMyOrg(sequence) {
		return nil // already approved
	}

	args := []string{
		"lifecycle", "chaincode", "approveformyorg",
		"-o", cc.OrdererAddr,
		"--ordererTLSHostnameOverride", fmt.Sprintf("orderer.%s", cc.NetworkDom),
		"--tls", "--cafile", cc.ordererCAPath(),
		"--channelID", cc.ChannelName,
		"--name", cc.ChaincodeName,
		"--version", cc.ChaincodeVer,
		"--package-id", packageID,
		"--sequence", fmt.Sprintf("%d", sequence),
	}
	if cc.SignaturePolicy != "" {
		args = append(args, "--signature-policy", cc.SignaturePolicy)
	}

	// Retry up to 2 minutes — large networks may need longer to catch up.
	const maxAttempts = 24
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cmd := exec.Command("peer", args...)
		cmd.Dir = cc.DeployDir
		cmd.Env = cc.peerEnv()
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		outStr := strings.TrimSpace(string(out))
		// Retry while peer is still applying channel config or transient network errors.
		if attempt < maxAttempts && isLifecycleSyncingError(outStr) {
			fmt.Printf("  ⏳ Peer/lifecycle not ready (attempt %d/%d)...\n", attempt, maxAttempts)
			time.Sleep(5 * time.Second)
			continue
		}
		return fmt.Errorf("approve chaincode: %s — %w", outStr, err)
	}
	return fmt.Errorf("approve chaincode: exceeded retry attempts")
}

// ────────────────────────────────────────────────────────────────────
// 5. Check commit readiness & commit
// ────────────────────────────────────────────────────────────────────

// CommittedSequence returns the latest committed sequence number for this
// chaincode, or 0 if not yet committed.
func (cc *ChaincodeOps) CommittedSequence() int {
	cmd := exec.Command("peer", "lifecycle", "chaincode", "querycommitted",
		"-C", cc.ChannelName,
		"-n", cc.ChaincodeName,
		"--output", "json")
	cmd.Dir = cc.DeployDir
	cmd.Env = cc.peerEnv()
	out, err := cmd.Output()
	if err != nil {
		return 0
	}

	var result struct {
		Sequence int `json:"sequence"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return 0
	}
	return result.Sequence
}

// CheckCommitReadiness returns a map of org MSPID → approved (bool).
func (cc *ChaincodeOps) CheckCommitReadiness(sequence int) map[string]bool {
	cmd := exec.Command("peer", cc.readinessArgs(sequence)...)
	cmd.Dir = cc.DeployDir
	cmd.Env = cc.peerEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}

	var result struct {
		Approvals map[string]bool `json:"approvals"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil
	}
	return result.Approvals
}

// CommitChaincode commits the chaincode definition to the channel.
// It sends the commit proposal to all known peer endpoints for cross-org endorsement.
func (cc *ChaincodeOps) CommitChaincode(sequence int) error {
	// Check if already committed at this sequence
	if cc.CommittedSequence() >= sequence {
		return nil // already committed
	}

	args := []string{
		"lifecycle", "chaincode", "commit",
		"-o", cc.OrdererAddr,
		"--ordererTLSHostnameOverride", fmt.Sprintf("orderer.%s", cc.NetworkDom),
		"--tls", "--cafile", cc.ordererCAPath(),
		"--channelID", cc.ChannelName,
		"--name", cc.ChaincodeName,
		"--version", cc.ChaincodeVer,
		"--sequence", fmt.Sprintf("%d", sequence),
	}
	if cc.SignaturePolicy != "" {
		args = append(args, "--signature-policy", cc.SignaturePolicy)
	}

	// Add peer endpoints for endorsement collection.
	// Each --peerAddresses needs a matching --tlsRootCertFiles.
	if len(cc.PeerEndpoints) > 0 {
		for _, ep := range cc.PeerEndpoints {
			if _, err := os.Stat(ep.TLSCertPath); err != nil {
				continue // skip if we don't have this peer's TLS cert
			}
			args = append(args, "--peerAddresses", ep.Addr, "--tlsRootCertFiles", ep.TLSCertPath)
		}
	}

	const maxAttempts = 12
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cmd := exec.Command("peer", args...)
		cmd.Dir = cc.DeployDir
		cmd.Env = cc.peerEnv()
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		outStr := strings.TrimSpace(string(out))
		if attempt < maxAttempts && isLifecycleSyncingError(outStr) {
			time.Sleep(5 * time.Second)
			continue
		}
		return fmt.Errorf("commit chaincode: %s — %w", outStr, err)
	}
	return fmt.Errorf("commit chaincode: exceeded retry attempts")
}

// ────────────────────────────────────────────────────────────────────
// 6. Chaincode container management
// ────────────────────────────────────────────────────────────────────

// IsCCContainerRunning checks if the chaincode container is running.
func (cc *ChaincodeOps) IsCCContainerRunning() bool {
	name := cc.ccContainerName()
	out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", name).Output()
	return strings.TrimSpace(string(out)) == "running"
}

// StartCCContainer starts the chaincode as a Docker container on the shared network.
// packageID is the Fabric package ID (used as CHAINCODE_ID env var).
func (cc *ChaincodeOps) StartCCContainer(packageID string) error {
	name := cc.ccContainerName()

	// If already running, nothing to do
	if cc.IsCCContainerRunning() {
		return nil
	}

	// Remove any stopped container with the same name
	exec.Command("docker", "rm", "-f", name).Run()

	// Run within the kufichain_network so the peer can reach it by container name
	cmd := exec.Command("docker", "run", "-d",
		"--name", name,
		"--network", "kufichain_network",
		"--restart", "unless-stopped",
		"-e", "CHAINCODE_ID="+packageID,
		"-e", "CHAINCODE_SERVER_ADDRESS=0.0.0.0:9999",
		cc.ChaincodeImageTag(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start chaincode container %s: %w", name, err)
	}
	return nil
}

// StopCCContainer stops and removes the chaincode container.
func (cc *ChaincodeOps) StopCCContainer() error {
	return exec.Command("docker", "rm", "-f", cc.ccContainerName()).Run()
}

// ────────────────────────────────────────────────────────────────────
// 7. High-level: EnsureChaincodeDeployed — one-call automation
// ────────────────────────────────────────────────────────────────────

// EnsureChaincodeDeployed performs the full chaincode lifecycle, handling both
// initial deployment and OR-policy upgrades:
//  1. Build Docker image (idempotent)
//  2. Create + install CCAAS package (idempotent)
//  3. Start chaincode container
//  4. Approve at target sequence (idempotent)
//  5. If commit-ready → commit
//
// Target sequence logic:
//   - Not yet committed → seq 1 (first deployment)
//   - Committed at seq N, SignaturePolicy set → seq N+1 (OR-policy upgrade)
//   - Committed at seq N, no SignaturePolicy → done (just restart container)
//
// It is safe to call on every `kufichain run`. Already-completed steps
// are skipped automatically.
func (cc *ChaincodeOps) EnsureChaincodeDeployed() error {
	committedSeq := cc.CommittedSequence()

	// Determine the target sequence.
	targetSeq := 1
	upgrading := false
	if committedSeq >= 1 {
		if cc.SignaturePolicy == "" {
			// Already committed, no upgrade configured — just ensure container is running.
			pkgID := cc.InstalledPackageID()
			if pkgID != "" {
				if err := cc.StartCCContainer(pkgID); err != nil {
					return fmt.Errorf("start chaincode container: %w", err)
				}
			}
			return nil
		}
		// OR-policy upgrade: target the next sequence.
		targetSeq = committedSeq + 1
		upgrading = true
		fmt.Printf("  Chaincode at seq %d — upgrading to seq %d with OR policy...\n", committedSeq, targetSeq)
	}

	// ── Build Docker image (idempotent — uses Docker layer cache) ─────
	if !upgrading {
		fmt.Printf("  Chaincode: building Docker image %s...\n", cc.ChaincodeImageTag())
	}
	if err := cc.BuildChaincodeImage(); err != nil {
		return fmt.Errorf("build chaincode image: %w", err)
	}
	if !upgrading {
		fmt.Println("  ✓ Chaincode image ready")
	}

	// ── Install package (idempotent — skips if already installed on peer) ─
	pkgID := cc.InstalledPackageID()
	if pkgID == "" {
		if !upgrading {
			fmt.Println("  Chaincode: creating CCAAS package...")
		}
		pkgPath, err := cc.BuildCCAASPackage()
		if err != nil {
			return fmt.Errorf("build CCAAS package: %w", err)
		}
		if !upgrading {
			fmt.Println("  ✓ CCAAS package ready")
			fmt.Println("  Chaincode: installing on peer...")
		}
		pkgID, err = cc.InstallChaincode(pkgPath)
		if err != nil {
			return fmt.Errorf("install chaincode: %w", err)
		}
		if !upgrading {
			fmt.Printf("  ✓ Installed (package ID: %s)\n", truncate(pkgID, 40))
		}
	}

	// ── Start chaincode container ─────────────────────────────────────
	if !upgrading {
		fmt.Println("  Chaincode: starting container...")
	}
	if err := cc.StartCCContainer(pkgID); err != nil {
		return fmt.Errorf("start chaincode container: %w", err)
	}
	if !upgrading {
		fmt.Printf("  ✓ Container %s running\n", cc.ccContainerName())
	}

	// Ensure lifecycle commands are usable before attempting approve/commit.
	if err := cc.WaitForLifecycleReady(targetSeq, 120); err != nil {
		return fmt.Errorf("wait for lifecycle readiness: %w", err)
	}

	// ── Approve at targetSeq (idempotent — skips if already approved) ─
	if !cc.IsApprovedForMyOrg(targetSeq) {
		if upgrading {
			fmt.Printf("  Chaincode: approving seq %d (OR policy) for %s...\n", targetSeq, cc.MSPID)
		} else {
			fmt.Printf("  Chaincode: approving for %s...\n", cc.MSPID)
		}
		if err := cc.ApproveChaincode(pkgID, targetSeq); err != nil {
			return fmt.Errorf("approve chaincode: %w", err)
		}
		fmt.Println("  ✓ Approved")
	}

	// ── Check readiness & commit if majority reached ───────────────────
	approvals := cc.CheckCommitReadiness(targetSeq)
	if approvals == nil {
		return fmt.Errorf("check commit readiness failed: peer lifecycle is not ready yet")
	}
	approvedCount := 0
	totalCount := len(approvals)
	for _, ok := range approvals {
		if ok {
			approvedCount++
		}
	}
	majority := totalCount/2 + 1

	if approvedCount >= majority {
		fmt.Println("  Chaincode: committing to channel...")
		if err := cc.CommitChaincode(targetSeq); err != nil {
			return fmt.Errorf("commit chaincode: %w", err)
		}
		if upgrading {
			fmt.Printf("  ✓ OR policy upgrade committed (seq %d)!\n", targetSeq)
		} else {
			fmt.Println("  ✓ Committed — chaincode is live!")
		}
	} else {
		fmt.Printf("  ⏳ Waiting for more approvals (%d/%d, need %d):\n", approvedCount, totalCount, majority)
		for msp, ok := range approvals {
			status := "✓ approved"
			if !ok {
				status = "⏳ pending"
			}
			fmt.Printf("      %s: %s\n", msp, status)
		}
		// Notify other peers' management APIs to trigger re-approval
		cc.notifyPeersToUpgrade()
		if upgrading {
			fmt.Printf("  OR policy upgrade (seq %d) will commit once majority approves.\n", targetSeq)
		} else {
			fmt.Println("  Chaincode will be committed once majority approves.")
		}
		fmt.Println("  (Peers are notified and will auto-approve shortly)")
	}

	return nil
}

// TryCommitIfReady checks if majority of orgs have approved and commits if so.
// If this org hasn't approved at the target sequence yet, it auto-approves first.
// Handles both initial deployment (seq 1) and OR-policy upgrades (seq N+1).
// Designed to be called periodically or after receiving a gossip event.
// Returns true once the target sequence is committed.
func (cc *ChaincodeOps) TryCommitIfReady() bool {
	committedSeq := cc.CommittedSequence()

	// Determine the target sequence.
	targetSeq := 1
	if committedSeq >= 1 {
		if cc.SignaturePolicy == "" {
			return true // already committed, no upgrade needed
		}
		targetSeq = committedSeq + 1
	}

	// Already committed at or beyond target?
	if committedSeq >= targetSeq {
		return true
	}

	if err := cc.WaitForLifecycleReady(targetSeq, 30); err != nil {
		return false
	}

	// Auto-approve at target sequence if this org hasn't done so yet.
	if !cc.IsApprovedForMyOrg(targetSeq) {
		pkgID := cc.InstalledPackageID()
		if pkgID == "" {
			return false // chaincode not installed on this peer
		}
		if err := cc.ApproveChaincode(pkgID, targetSeq); err != nil {
			return false
		}
		fmt.Printf("  ✓ Auto-approved chaincode at seq %d with OR policy\n", targetSeq)
	}

	approvals := cc.CheckCommitReadiness(targetSeq)
	if approvals == nil {
		return false
	}
	approvedCount := 0
	for _, ok := range approvals {
		if ok {
			approvedCount++
		}
	}
	if approvedCount < len(approvals)/2+1 {
		return false
	}

	return cc.CommitChaincode(targetSeq) == nil
}

// notifyPeersToUpgrade calls /api/chaincode/trigger-upgrade on all known
// peer management APIs so they auto-approve at the new sequence.  Best-effort.
func (cc *ChaincodeOps) notifyPeersToUpgrade() {
	for _, ep := range cc.PeerEndpoints {
		if ep.MgmtAddr == "" {
			continue
		}
		go func(addr string) {
			url := addr + "/api/chaincode/trigger-upgrade"
			resp, err := http.Post(url, "application/json", nil)
			if err != nil {
				return
			}
			resp.Body.Close()
		}(ep.MgmtAddr)
	}
}

// ────────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
