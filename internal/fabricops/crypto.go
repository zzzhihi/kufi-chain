// Package fabricops provides operations for managing Fabric infrastructure:
// crypto material generation, Docker container management, and channel operations.
package fabricops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// GenerateBootstrapCrypto creates crypto material for orderer + first org.
func GenerateBootstrapCrypto(deployDir, orgName, domain, networkDomain, externalHost string) error {
	ordererSANs := yamlSANEntries("localhost", "127.0.0.1", "orderer."+networkDomain)
	peerSANs := yamlSANEntries("localhost", "127.0.0.1", "peer0."+domain, externalHost)
	cryptoCfg := fmt.Sprintf(`OrdererOrgs:
  - Name: Orderer
    Domain: %s
    EnableNodeOUs: true
    Specs:
      - Hostname: orderer
        SANS:
%s

PeerOrgs:
  - Name: %s
    Domain: %s
    EnableNodeOUs: true
    Template:
      Count: 1
      SANS:
%s
    Users:
      Count: 1
`, networkDomain, ordererSANs, orgName, domain, peerSANs)

	cfgPath := filepath.Join(deployDir, "crypto-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cryptoCfg), 0o644); err != nil {
		return fmt.Errorf("write crypto-config.yaml: %w", err)
	}

	outputDir := filepath.Join(deployDir, "crypto-config")
	os.RemoveAll(outputDir) // clean previous

	return runCmd(deployDir, "cryptogen", "generate",
		"--config=crypto-config.yaml",
		"--output=crypto-config")
}

// GenerateOrdererOnlyCrypto creates crypto material for a dedicated orderer node.
// Only generates OrdererOrg — no peer orgs.
func GenerateOrdererOnlyCrypto(deployDir, networkDomain, externalHost string) error {
	ordererSANs := yamlSANEntries("localhost", "127.0.0.1", "orderer."+networkDomain, externalHost)
	cryptoCfg := fmt.Sprintf(`OrdererOrgs:
  - Name: Orderer
    Domain: %s
    EnableNodeOUs: true
    Specs:
      - Hostname: orderer
        SANS:
%s
`, networkDomain, ordererSANs)

	cfgPath := filepath.Join(deployDir, "crypto-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cryptoCfg), 0o644); err != nil {
		return fmt.Errorf("write crypto-config.yaml: %w", err)
	}

	outputDir := filepath.Join(deployDir, "crypto-config")
	os.RemoveAll(outputDir) // clean previous

	return runCmd(deployDir, "cryptogen", "generate",
		"--config=crypto-config.yaml",
		"--output=crypto-config")
}

// GenerateOrgCrypto creates crypto material for a single joining org.
func GenerateOrgCrypto(deployDir, orgName, domain, externalHost string) error {
	peerSANs := yamlSANEntries("localhost", "127.0.0.1", "peer0."+domain, externalHost)
	cryptoCfg := fmt.Sprintf(`PeerOrgs:
  - Name: %s
    Domain: %s
    EnableNodeOUs: true
    Template:
      Count: 1
      SANS:
%s
    Users:
      Count: 1
`, orgName, domain, peerSANs)

	cfgPath := filepath.Join(deployDir, "crypto-config-join.yaml")
	if err := os.WriteFile(cfgPath, []byte(cryptoCfg), 0o644); err != nil {
		return fmt.Errorf("write crypto-config-join.yaml: %w", err)
	}

	return runCmd(deployDir, "cryptogen", "generate",
		"--config=crypto-config-join.yaml",
		"--output=crypto-config")
}

func yamlSANEntries(values ...string) string {
	seen := make(map[string]struct{}, len(values))
	lines := make([]string, 0, len(values))
	for _, value := range values {
		v := strings.TrimSpace(value)
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			if parsed, err := neturlParse(v); err == nil {
				v = parsed
			}
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		lines = append(lines, "        - "+v)
	}
	return strings.Join(lines, "\n")
}

func neturlParse(raw string) (string, error) {
	host := raw
	if idx := strings.Index(raw, "://"); idx >= 0 {
		host = raw[idx+3:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h, nil
	}
	return host, nil
}

// GenerateOrgDefinitionJSON runs configtxgen -printOrg to produce the JSON
// definition needed for channel config updates.
func GenerateOrgDefinitionJSON(deployDir, orgName, mspID, domain, networkDomain string, anchorHost string, anchorPort int) (string, error) {
	// Create a temporary configtx.yaml for the new org
	configtx := fmt.Sprintf(`Organizations:
  - &%s
    Name: %s
    ID: %s
    MSPDir: crypto-config/peerOrganizations/%s/msp
    Policies:
      Readers:
        Type: Signature
        Rule: "OR('%s.admin', '%s.peer', '%s.client')"
      Writers:
        Type: Signature
        Rule: "OR('%s.admin', '%s.client')"
      Admins:
        Type: Signature
        Rule: "OR('%s.admin')"
      Endorsement:
        Type: Signature
        Rule: "OR('%s.peer')"
    AnchorPeers:
      - Host: %s
        Port: %d
`, orgName, mspID, mspID, domain,
		mspID, mspID, mspID,
		mspID, mspID,
		mspID,
		mspID,
		anchorHost, anchorPort)

	tmpDir, err := os.MkdirTemp("", "kufichain-configtx-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "configtx.yaml"), []byte(configtx), 0o644); err != nil {
		return "", err
	}

	// configtxgen needs FABRIC_CFG_PATH pointing to the dir with configtx.yaml
	// and it resolves MSPDir relative to FABRIC_CFG_PATH
	cmd := exec.Command("configtxgen", "-printOrg", mspID)
	cmd.Dir = deployDir
	cmd.Env = append(os.Environ(), "FABRIC_CFG_PATH="+tmpDir)

	// But MSPDir in configtx is relative — we need it relative to tmpDir
	// So symlink deploy's crypto-config into tmpDir
	srcCrypto := filepath.Join(deployDir, "crypto-config")
	dstCrypto := filepath.Join(tmpDir, "crypto-config")
	if err := os.Symlink(srcCrypto, dstCrypto); err != nil {
		return "", fmt.Errorf("symlink crypto: %w", err)
	}

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("configtxgen -printOrg: %s", string(ee.Stderr))
		}
		return "", fmt.Errorf("configtxgen -printOrg: %w", err)
	}
	return string(out), nil
}

// PackageMSPDir creates a base64-encoded tar.gz of an org's MSP directory.
func PackageMSPDir(mspDir string) (string, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	baseDir := filepath.Dir(mspDir)
	relBase := filepath.Base(mspDir)

	err := filepath.Walk(mspDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(baseDir, path)
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if info.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("tar walk %s: %w", relBase, err)
	}

	tw.Close()
	gw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// UnpackMSPBundle extracts a base64 tar.gz bundle into the target directory.
func UnpackMSPBundle(b64Data, targetDir string) error {
	data, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return fmt.Errorf("base64 decode: %w", err)
	}

	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(targetDir, hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			os.MkdirAll(target, 0o755)
			continue
		}

		os.MkdirAll(filepath.Dir(target), 0o755)
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		io.Copy(f, tr)
		f.Close()
	}
	return nil
}

// --- Configtx generation for bootstrap ---

// configtxOrdererOnlyTmpl is used when setting up a dedicated orderer node.
// OrdererOrg is placed ONLY in the Orderer section; the Application section
// starts empty.  Peer orgs get added to Application via channel config updates.
// Keeping OrdererMSP out of Application avoids it counting toward chaincode
// lifecycle endorsement majority.
const configtxOrdererOnlyTmpl = `Organizations:
  - &OrdererOrg
    Name: OrdererMSP
    ID: OrdererMSP
    MSPDir: crypto-config/ordererOrganizations/{{.NetworkDomain}}/msp
    Policies:
      Readers:
        Type: Signature
        Rule: "OR('OrdererMSP.member')"
      Writers:
        Type: Signature
        Rule: "OR('OrdererMSP.member')"
      Admins:
        Type: Signature
        Rule: "OR('OrdererMSP.admin')"
    OrdererEndpoints:
      - orderer.{{.NetworkDomain}}:7050

Capabilities:
  Channel: &ChannelCapabilities
    V2_0: true
  Orderer: &OrdererCapabilities
    V2_0: true
  Application: &ApplicationCapabilities
    V2_0: true

Application: &ApplicationDefaults
  Organizations:
  Policies:
    Readers:
      Type: ImplicitMeta
      Rule: "ANY Readers"
    Writers:
      Type: ImplicitMeta
      Rule: "ANY Writers"
    Admins:
      Type: ImplicitMeta
      Rule: "MAJORITY Admins"
    LifecycleEndorsement:
      Type: ImplicitMeta
      Rule: "MAJORITY Endorsement"
    Endorsement:
      Type: ImplicitMeta
      Rule: "MAJORITY Endorsement"
  Capabilities:
    <<: *ApplicationCapabilities

Orderer: &OrdererDefaults
  OrdererType: etcdraft
  Addresses:
    - orderer.{{.NetworkDomain}}:7050
  BatchTimeout: 2s
  BatchSize:
    MaxMessageCount: 500
    AbsoluteMaxBytes: 99 MB
    PreferredMaxBytes: 512 KB
  EtcdRaft:
    Consenters:
      - Host: orderer.{{.NetworkDomain}}
        Port: 7050
        ClientTLSCert: crypto-config/ordererOrganizations/{{.NetworkDomain}}/orderers/orderer.{{.NetworkDomain}}/tls/server.crt
        ServerTLSCert: crypto-config/ordererOrganizations/{{.NetworkDomain}}/orderers/orderer.{{.NetworkDomain}}/tls/server.crt
  Organizations:
  Policies:
    Readers:
      Type: ImplicitMeta
      Rule: "ANY Readers"
    Writers:
      Type: ImplicitMeta
      Rule: "ANY Writers"
    Admins:
      Type: ImplicitMeta
      Rule: "MAJORITY Admins"
    BlockValidation:
      Type: ImplicitMeta
      Rule: "ANY Writers"

Channel: &ChannelDefaults
  Policies:
    Readers:
      Type: ImplicitMeta
      Rule: "ANY Readers"
    Writers:
      Type: ImplicitMeta
      Rule: "ANY Writers"
    Admins:
      Type: ImplicitMeta
      Rule: "MAJORITY Admins"
  Capabilities:
    <<: *ChannelCapabilities

Profiles:
  ChannelGenesis:
    <<: *ChannelDefaults
    Orderer:
      <<: *OrdererDefaults
      Organizations:
        - *OrdererOrg
      Capabilities:
        <<: *OrdererCapabilities
    Application:
      <<: *ApplicationDefaults
      Organizations: []
      Capabilities:
        <<: *ApplicationCapabilities
`

const configtxBootstrapTmpl = `Organizations:
  - &OrdererOrg
    Name: OrdererMSP
    ID: OrdererMSP
    MSPDir: crypto-config/ordererOrganizations/{{.NetworkDomain}}/msp
    Policies:
      Readers:
        Type: Signature
        Rule: "OR('OrdererMSP.member')"
      Writers:
        Type: Signature
        Rule: "OR('OrdererMSP.member')"
      Admins:
        Type: Signature
        Rule: "OR('OrdererMSP.admin')"
    OrdererEndpoints:
      - orderer.{{.NetworkDomain}}:7050

  - &{{.OrgName}}
    Name: {{.MSPID}}
    ID: {{.MSPID}}
    MSPDir: crypto-config/peerOrganizations/{{.Domain}}/msp
    Policies:
      Readers:
        Type: Signature
        Rule: "OR('{{.MSPID}}.admin', '{{.MSPID}}.peer', '{{.MSPID}}.client')"
      Writers:
        Type: Signature
        Rule: "OR('{{.MSPID}}.admin', '{{.MSPID}}.client')"
      Admins:
        Type: Signature
        Rule: "OR('{{.MSPID}}.admin')"
      Endorsement:
        Type: Signature
        Rule: "OR('{{.MSPID}}.peer')"
    AnchorPeers:
      - Host: peer0.{{.Domain}}
        Port: {{.PeerPort}}

Capabilities:
  Channel: &ChannelCapabilities
    V2_0: true
  Orderer: &OrdererCapabilities
    V2_0: true
  Application: &ApplicationCapabilities
    V2_0: true

Application: &ApplicationDefaults
  Organizations:
  Policies:
    Readers:
      Type: ImplicitMeta
      Rule: "ANY Readers"
    Writers:
      Type: ImplicitMeta
      Rule: "ANY Writers"
    Admins:
      Type: ImplicitMeta
      Rule: "MAJORITY Admins"
    LifecycleEndorsement:
      Type: ImplicitMeta
      Rule: "MAJORITY Endorsement"
    Endorsement:
      Type: ImplicitMeta
      Rule: "MAJORITY Endorsement"
  Capabilities:
    <<: *ApplicationCapabilities

Orderer: &OrdererDefaults
  OrdererType: etcdraft
  Addresses:
    - orderer.{{.NetworkDomain}}:7050
  BatchTimeout: 2s
  BatchSize:
    MaxMessageCount: 500
    AbsoluteMaxBytes: 99 MB
    PreferredMaxBytes: 512 KB
  EtcdRaft:
    Consenters:
      - Host: orderer.{{.NetworkDomain}}
        Port: 7050
        ClientTLSCert: crypto-config/ordererOrganizations/{{.NetworkDomain}}/orderers/orderer.{{.NetworkDomain}}/tls/server.crt
        ServerTLSCert: crypto-config/ordererOrganizations/{{.NetworkDomain}}/orderers/orderer.{{.NetworkDomain}}/tls/server.crt
  Organizations:
  Policies:
    Readers:
      Type: ImplicitMeta
      Rule: "ANY Readers"
    Writers:
      Type: ImplicitMeta
      Rule: "ANY Writers"
    Admins:
      Type: ImplicitMeta
      Rule: "MAJORITY Admins"
    BlockValidation:
      Type: ImplicitMeta
      Rule: "ANY Writers"

Channel: &ChannelDefaults
  Policies:
    Readers:
      Type: ImplicitMeta
      Rule: "ANY Readers"
    Writers:
      Type: ImplicitMeta
      Rule: "ANY Writers"
    Admins:
      Type: ImplicitMeta
      Rule: "MAJORITY Admins"
  Capabilities:
    <<: *ChannelCapabilities

Profiles:
  ChannelGenesis:
    <<: *ChannelDefaults
    Orderer:
      <<: *OrdererDefaults
      Organizations:
        - *OrdererOrg
      Capabilities:
        <<: *OrdererCapabilities
    Application:
      <<: *ApplicationDefaults
      Organizations:
        - *{{.OrgName}}
      Capabilities:
        <<: *ApplicationCapabilities
`

// GenerateConfigtxBootstrap creates configtx.yaml for the initial network.
func GenerateConfigtxBootstrap(deployDir string, data map[string]interface{}) error {
	tmpl, err := template.New("configtx").Parse(configtxBootstrapTmpl)
	if err != nil {
		return err
	}

	path := filepath.Join(deployDir, "configtx.yaml")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}

// GenerateConfigtxOrdererOnly creates configtx.yaml for a dedicated orderer node.
// OrdererOrg is in both Orderer and Application sections so it can sign config updates.
func GenerateConfigtxOrdererOnly(deployDir string, data map[string]interface{}) error {
	tmpl, err := template.New("configtx").Parse(configtxOrdererOnlyTmpl)
	if err != nil {
		return err
	}

	path := filepath.Join(deployDir, "configtx.yaml")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}

// GenerateGenesisBlock runs configtxgen to create the channel genesis block.
func GenerateGenesisBlock(deployDir, channelName string) error {
	artifactsDir := filepath.Join(deployDir, "channel-artifacts")
	os.MkdirAll(artifactsDir, 0o755)

	return runCmd(deployDir, "configtxgen",
		"-profile", "ChannelGenesis",
		"-outputBlock", filepath.Join("channel-artifacts", channelName+".block"),
		"-channelID", channelName)
}

// discoverFabricBinDir searches common locations for Fabric binaries and
// prepends the directory to PATH so all subsequent exec calls find them.
func discoverFabricBinDir() string {
	// Already in PATH?
	if _, err := exec.LookPath("peer"); err == nil {
		return ""
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		"/tmp/bin",
		filepath.Join(home, "fabric-samples", "bin"),
		filepath.Join(home, "fabric", "bin"),
		filepath.Join(home, ".local", "bin"),
		"/usr/local/fabric/bin",
		"/opt/fabric/bin",
	}

	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "peer")); err == nil {
			return dir
		}
	}
	return ""
}

// SetupFabricPath finds Fabric binaries and adds them to PATH.
// Returns the discovered directory (empty if already in PATH).
func SetupFabricPath() string {
	dir := discoverFabricBinDir()
	if dir == "" {
		return ""
	}
	curPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+curPath)
	return dir
}

// CheckPrerequisites verifies that required binaries are available.
func CheckPrerequisites() error {
	bins := []string{"cryptogen", "configtxgen", "configtxlator", "osnadmin", "peer", "docker", "jq"}
	var missing []string
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required binaries: %s", strings.Join(missing, ", "))
	}
	return nil
}

func runCmd(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FABRIC_CFG_PATH="+dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func runCmdOutput(dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "FABRIC_CFG_PATH="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %s — %w", name, string(out), err)
	}
	return out, nil
}
