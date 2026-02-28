// cmd/kufichain/main.go — KufiChain: Decentralized Fabric Payment Network CLI
//
// Zero-config node management. All crypto, configs, and docker files are
// auto-generated at runtime. Stakeholders only need to:
//
//	./install-prereq.sh                         # Install prerequisites
//	./kufichain setup                           # Bootstrap a new network
//	./kufichain join --bootstrap 10.0.1.5:9500  # Join existing network
//	./kufichain run                             # Start the payment gateway
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fabric-payment-gateway/internal/fabricops"
	"github.com/fabric-payment-gateway/internal/nodemgr"
)

const (
	defaultDataDir = ".kufichain"
	networkDomain  = "kufichain.network"
	defaultNet     = "kufichain"
)

type peerSetupOptions struct {
	OrgName      string
	ExternalHost string
	PeerPort     int
	CouchDBPort  int
	GatewayPort  int
	OpsPort      int
	MgmtPort     int
	AssumeYes    bool
}

// parseDataDir extracts --data-dir flag from os.Args (before subcommand flags).
// Returns the data dir path and the remaining args.
func parseDataDir() string {
	for i, arg := range os.Args {
		if arg == "--data-dir" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
		if strings.HasPrefix(arg, "--data-dir=") {
			return strings.TrimPrefix(arg, "--data-dir=")
		}
	}
	return ""
}

// resolveDataDir returns an absolute path for the node's data directory.
func resolveDataDir() string {
	custom := parseDataDir()
	projectDir := findProjectRoot()
	if custom != "" {
		if filepath.IsAbs(custom) {
			return custom
		}
		return filepath.Join(projectDir, custom)
	}
	return filepath.Join(projectDir, defaultDataDir)
}

func main() {
	// Ensure Fabric binaries are in PATH for ALL commands (setup, dashboard, run, etc.)
	fabricops.SetupFabricPath()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "setup":
		cmdSetup()
	case "join":
		cmdJoin()
	case "run":
		cmdRun()
	case "status":
		cmdStatus()
	case "stop":
		cmdStop()
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`kufichain — Decentralized Fabric Payment Network

Commands:
  setup                              Set up a new node (orderer or peer)
  join  --bootstrap <host:port>      Join as peer (supports non-interactive flags)
  run                                Start node + interactive dashboard
  status                             Quick status check
  stop                               Stop this node's containers

Global flag:
  --data-dir <path>                  Node data directory (default: .kufichain)
                                     Use different dirs to run multiple nodes locally:
                                       ./kufichain setup --data-dir .orderer
                                       ./kufichain setup --data-dir .peer1

Quick start:
  1. ./install-prereq.sh                          # Install Docker, Fabric, Go
  2. ./kufichain setup                            # Choose: Orderer (one node) or Peer
  3. ./kufichain run                              # Start & manage the node

One-command peer join (for EC2 automation):
  ./kufichain join --bootstrap 10.0.1.5:9500 --org BankA --host 52.10.10.10 --yes`)
}

// =====================================================================
// SETUP — Interactive node setup with role selection
// =====================================================================

func cmdSetup() {
	reader := bufio.NewReader(os.Stdin)

	printBanner("KufiChain — Node Setup")

	// Check prerequisites
	fmt.Println("Checking prerequisites...")
	if binDir := fabricops.SetupFabricPath(); binDir != "" {
		fmt.Printf("  Found Fabric binaries in %s\n", binDir)
	}
	if err := fabricops.CheckPrerequisites(); err != nil {
		fmt.Println()
		fmt.Println("  Missing binaries detected. Run ./install-prereq.sh first.")
		fatal("Prerequisites check failed: %v", err)
	}
	fmt.Println("  ✓ All required binaries found")

	// Check if already setup
	dataDir := resolveDataDir()
	store := nodemgr.NewStore(dataDir)
	if store.NodeConfigExists() {
		fmt.Printf("  Node already initialized in %s.\n\n", dataDir)
		fmt.Println("    1) Run    — Start the node (same as 'kufichain run')")
		fmt.Println("    2) Reset  — Delete existing data and set up from scratch")
		fmt.Println()
		choice := prompt(reader, "Choice (1 or 2)", "1")
		switch choice {
		case "1", "run":
			nodeCfg, err := store.LoadNodeConfig()
			if err != nil {
				fatal("Load config: %v", err)
			}
			startDashboard(store, nodeCfg)
			return
		case "2", "reset":
			nodeCfg, _ := store.LoadNodeConfig()
			if nodeCfg != nil {
				fmt.Println("  Stopping existing containers...")
				fabricops.StopContainers(nodeCfg.DeployDir)
				if err := fabricops.CleanupContainers(nodeCfg.DeployDir); err != nil {
					fmt.Fprintf(os.Stderr, "  Warning: cleanup containers: %v\n", err)
				}
			}
			fmt.Printf("  Removing %s...\n", dataDir)
			os.RemoveAll(dataDir)
			store = nodemgr.NewStore(dataDir)
			fmt.Println("  ✓ Reset complete. Continuing with fresh setup...")
		default:
			fatal("Invalid choice.")
		}
	}

	// Role selection
	fmt.Println()
	fmt.Println("  Select this node's role:")
	fmt.Println()
	fmt.Println("    1) Orderer — Dedicated ordering service (1-2 per network)")
	fmt.Println("       Creates the network. No peer, no gateway.")
	fmt.Println()
	fmt.Println("    2) Peer    — Organization's peer node")
	fmt.Println("       Joins an existing network. Runs peer + gateway.")
	fmt.Println()
	roleChoice := prompt(reader, "Role (1 or 2)", "")
	switch roleChoice {
	case "1", "orderer":
		setupOrderer(reader, dataDir, store)
	case "2", "peer":
		setupPeer(reader, dataDir, store)
	default:
		fatal("Invalid choice. Enter 1 (Orderer) or 2 (Peer).")
	}
}

// =====================================================================
// SETUP ORDERER — Dedicated ordering service
// =====================================================================

func setupOrderer(reader *bufio.Reader, dataDir string, store *nodemgr.Store) {
	printBanner("KufiChain — Orderer Setup")

	fmt.Println("  This node will run a dedicated orderer for the network.")
	fmt.Println("  You typically need only 1-2 orderers for the entire network.")
	fmt.Println()

	externalHost := prompt(reader, "This node's public IP or hostname", "")
	if externalHost == "" {
		fatal("Public IP/hostname is required for peers to connect")
	}
	channelName := prompt(reader, "Channel name", "payment-channel")

	// Port configuration
	fmt.Println()
	fmt.Println("  Port configuration (press Enter for defaults):")
	ordererPort := promptInt(reader, "Orderer port", 7050)
	ordererAdminPort := promptInt(reader, "Orderer admin port (osnadmin)", 7053)
	opsPort := promptInt(reader, "Operations/metrics port", 9443)
	mgmtPort := promptInt(reader, "Management API port", 9500)

	deployDir := filepath.Join(dataDir, "deploy")
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		fatal("Create deploy dir: %v", err)
	}

	nodeCfg := &nodemgr.NodeConfig{
		Role:             nodemgr.RoleOrderer,
		OrgName:          "Orderer",
		MSPID:            "OrdererMSP",
		Domain:           networkDomain,
		NetworkDomain:    networkDomain,
		ChannelName:      channelName,
		OrdererPort:      ordererPort,
		OrdererAdminPort: ordererAdminPort,
		OpsPort:          opsPort,
		MgmtPort:         mgmtPort,
		OrdererAddr:      fmt.Sprintf("localhost:%d", ordererPort),
		ExternalHost:     externalHost,
		DataDir:          dataDir,
		DeployDir:        deployDir,
	}

	// Confirm
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────┐")
	fmt.Printf("│  Role:     %-32s │\n", "Orderer (dedicated)")
	fmt.Printf("│  Channel:  %-32s │\n", channelName)
	fmt.Printf("│  Host:     %-32s │\n", externalHost)
	fmt.Printf("│  Orderer:  %-32s │\n", fmt.Sprintf(":%d", ordererPort))
	fmt.Printf("│  Admin:    %-32s │\n", fmt.Sprintf(":%d (osnadmin)", ordererAdminPort))
	fmt.Printf("│  Ops:      %-32s │\n", fmt.Sprintf(":%d", opsPort))
	fmt.Printf("│  Mgmt API: %-32s │\n", fmt.Sprintf(":%d", mgmtPort))
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  Crypto keys, TLS certs, and configs will be auto-generated.")
	fmt.Println("  No peer or CouchDB will be deployed on this node.")
	confirm := prompt(reader, "\n  Proceed? (y/n)", "y")
	if strings.ToLower(confirm) != "y" {
		fmt.Println("Aborted.")
		return
	}

	// Initialize store
	if err := store.Init(); err != nil {
		fatal("Init store: %v", err)
	}
	if err := store.SaveNodeConfig(nodeCfg); err != nil {
		fatal("Save node config: %v", err)
	}

	// Step 1: Generate orderer crypto (OrdererOrg only)
	step("1/5", "Generating crypto material (orderer keys, TLS certs)...")
	if err := fabricops.GenerateOrdererOnlyCrypto(deployDir, networkDomain); err != nil {
		fatal("Crypto generation: %v", err)
	}
	fmt.Println("  ✓ Orderer crypto auto-generated")

	// Step 2: Generate channel config (orderer-only genesis)
	step("2/5", "Generating channel configuration...")
	tmplData := map[string]interface{}{
		"NetworkDomain": networkDomain,
	}
	if err := fabricops.GenerateConfigtxOrdererOnly(deployDir, tmplData); err != nil {
		fatal("Configtx generation: %v", err)
	}
	if err := fabricops.GenerateGenesisBlock(deployDir, channelName); err != nil {
		fatal("Genesis block: %v", err)
	}
	fmt.Println("  ✓ Channel genesis block created")

	// Step 3: Ensure orderer.yaml + core.yaml (core.yaml needed for peer CLI on this node)
	if err := fabricops.EnsureOrdererConfig(deployDir); err != nil {
		fatal("Generate orderer config: %v", err)
	}
	if err := fabricops.EnsureFabricConfig(deployDir); err != nil {
		fatal("Generate Fabric config: %v", err)
	}

	// Step 4: Generate docker-compose (orderer only)
	step("3/5", "Generating docker-compose (orderer only)...")
	dcCfg := fabricops.DockerConfig{
		NetworkName:      defaultNet,
		NetworkDomain:    networkDomain,
		OrdererPort:      ordererPort,
		OrdererAdminPort: ordererAdminPort,
		OrdererOpsPort:   opsPort,
	}
	if err := fabricops.GenerateOrdererCompose(deployDir, dcCfg); err != nil {
		fatal("Docker compose: %v", err)
	}
	fmt.Println("  ✓ docker-compose.yaml generated (orderer only, no peer/CouchDB)")

	// Step 5: Start orderer container
	step("4/5", "Starting orderer container...")

	// Check for existing containers from a previous run
	if running := fabricops.ContainersRunning(deployDir); len(running) > 0 {
		fmt.Printf("  ⚠ Existing containers found: %s\n", strings.Join(running, ", "))
		choice := prompt(reader, "  (r)euse them or (d)elete and recreate?", "d")
		if strings.ToLower(choice) == "r" {
			fmt.Println("  ✓ Reusing existing containers")
		} else {
			if err := fabricops.CleanupContainers(deployDir); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: cleanup containers: %v\n", err)
			}
			if err := fabricops.StartContainers(deployDir); err != nil {
				fatal("Start containers: %v", err)
			}
			fmt.Println("  ✓ Container started")
		}
	} else {
		if err := fabricops.CleanupContainers(deployDir); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: cleanup containers: %v\n", err)
		}
		if err := fabricops.StartContainers(deployDir); err != nil {
			fatal("Start containers: %v", err)
		}
		fmt.Println("  ✓ Container started")
	}

	// Wait for orderer
	fmt.Print("  Waiting for orderer")
	if !waitForService(deployDir, "orderer."+networkDomain, 30) {
		fatal("Orderer failed to start. Check compose logs in %s for service orderer.%s", deployDir, networkDomain)
	}
	fmt.Println(" ready")

	// Step 6: Create channel
	step("5/5", "Creating channel...")
	chOps := &fabricops.ChannelOps{
		DeployDir:      deployDir,
		ChannelName:    channelName,
		OrdererAddr:    fmt.Sprintf("localhost:%d", ordererPort),
		NetworkDom:     networkDomain,
		OrgName:        "Orderer",
		MSPID:          "OrdererMSP",
		Domain:         networkDomain,
		AdminPort:      ordererAdminPort,
		IsOrdererAdmin: true,
	}
	if err := chOps.WaitForOrdererAdminReady(30); err != nil {
		fatal("Orderer admin API not ready: %v", err)
	}
	if err := chOps.JoinOrdererToChannel(); err != nil {
		fatal("Join orderer to channel: %v", err)
	}
	fmt.Println("  ✓ Channel created on orderer")

	// Initialize empty peer list
	if err := store.SavePeers(nil); err != nil {
		fatal("Save initial peer list: %v", err)
	}

	// Done — auto-start the dashboard
	fmt.Println()
	fmt.Println("  ╔═══════════════════════════════════════════════════════╗")
	fmt.Println("  ║  ✓ Orderer is running!                                ║")
	fmt.Println("  ╠═══════════════════════════════════════════════════════╣")
	fmt.Println("  ║  Peer nodes can join with:                            ║")
	fmt.Printf("  ║    ./kufichain setup      (choose Peer, bootstrap     ║\n")
	fmt.Printf("  ║      address: %s:%d)                \n", externalHost, mgmtPort)
	fmt.Println("  ║  Or:                                                  ║")
	fmt.Printf("  ║    ./kufichain join --bootstrap %s:%d \n", externalHost, mgmtPort)
	fmt.Println("  ╚═══════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("  Starting dashboard to manage peer join requests...")
	fmt.Println()
	startDashboard(store, nodeCfg)
}

// =====================================================================
// SETUP PEER — Join existing network as a peer node
// =====================================================================

func setupPeer(reader *bufio.Reader, dataDir string, store *nodemgr.Store) {
	printBanner("KufiChain — Peer Setup")

	fmt.Println("  This node will join an existing network as an organization's peer.")
	fmt.Println("  You need the orderer's management API address to proceed.")
	fmt.Println()

	// Ask for bootstrap address
	addr := prompt(reader, "Orderer management API address (host:port)", "")
	if addr == "" {
		fatal("Bootstrap address is required. Get it from the orderer operator.")
	}
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}

	setupPeerWithBootstrap(reader, dataDir, store, addr, nil)
}

// =====================================================================
// JOIN — Shortcut to join as peer (same as setup → peer)
// =====================================================================

func cmdJoin() {
	// Parse --bootstrap flag
	joinFlags := flag.NewFlagSet("join", flag.ExitOnError)
	bootstrapAddr := joinFlags.String("bootstrap", "", "Orderer management API address (host:port)")
	orgName := joinFlags.String("org", "", "Organization/bank name (non-interactive mode)")
	externalHost := joinFlags.String("host", "", "Public IP/hostname of this node (non-interactive mode)")
	peerPort := joinFlags.Int("peer-port", 7051, "Peer listen port")
	couchDBPort := joinFlags.Int("couchdb-port", 5984, "CouchDB port")
	gatewayPort := joinFlags.Int("gateway-port", 8080, "Gateway HTTP port")
	opsPort := joinFlags.Int("ops-port", 9444, "Operations/metrics port")
	mgmtPort := joinFlags.Int("mgmt-port", 9500, "Management API port")
	assumeYes := joinFlags.Bool("yes", false, "Skip confirmation prompt (for automation)")
	_ = joinFlags.String("data-dir", "", "") // consumed by parseDataDir, ignore here
	joinFlags.Parse(os.Args[2:])

	if *bootstrapAddr == "" {
		fmt.Println("Usage: kufichain join --bootstrap <host:port>")
		fmt.Println("Example: kufichain join --bootstrap 10.0.1.5:9500")
		fmt.Println("Automation example:")
		fmt.Println("  kufichain join --bootstrap 10.0.1.5:9500 --org BankA --host 52.10.10.10 --yes")
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	// Check prerequisites
	fmt.Println("Checking prerequisites...")
	if binDir := fabricops.SetupFabricPath(); binDir != "" {
		fmt.Printf("  Found Fabric binaries in %s\n", binDir)
	}
	if err := fabricops.CheckPrerequisites(); err != nil {
		fmt.Println()
		fmt.Println("  Missing binaries detected. Run ./install-prereq.sh first.")
		fatal("Prerequisites check failed: %v", err)
	}
	fmt.Println("  ✓ All required binaries found")

	// Check already initialized
	dataDir := resolveDataDir()
	store := nodemgr.NewStore(dataDir)
	if store.NodeConfigExists() {
		fmt.Printf("  Node already initialized in %s. Starting node...\n", dataDir)
		nodeCfg, err := store.LoadNodeConfig()
		if err != nil {
			fatal("Load config: %v", err)
		}
		startDashboard(store, nodeCfg)
		return
	}

	addr := *bootstrapAddr
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}

	opts := &peerSetupOptions{
		OrgName:      strings.TrimSpace(*orgName),
		ExternalHost: strings.TrimSpace(*externalHost),
		PeerPort:     *peerPort,
		CouchDBPort:  *couchDBPort,
		GatewayPort:  *gatewayPort,
		OpsPort:      *opsPort,
		MgmtPort:     *mgmtPort,
		AssumeYes:    *assumeYes,
	}
	// Interactive mode unless required fields are provided for automation.
	if opts.OrgName == "" && opts.ExternalHost == "" {
		opts = nil
	}

	setupPeerWithBootstrap(reader, dataDir, store, addr, opts)
}

// =====================================================================
// setupPeerWithBootstrap — shared logic for peer setup (used by both
// cmdSetup → peer and cmdJoin)
// =====================================================================

func setupPeerWithBootstrap(reader *bufio.Reader, dataDir string, store *nodemgr.Store, addr string, opts *peerSetupOptions) {
	printBanner("KufiChain — Peer Setup")

	// Verify connectivity to orderer management API — retry for up to 60s
	// in case the orderer is still initializing.
	fmt.Printf("  Connecting to orderer management API %s...\n", addr)
	client := &http.Client{Timeout: 10 * time.Second}
	var bootstrapStatus map[string]interface{}
	const maxRetries = 6
	for i := 1; i <= maxRetries; i++ {
		resp, err := client.Get(addr + "/api/status")
		if err == nil {
			decodeErr := json.NewDecoder(resp.Body).Decode(&bootstrapStatus)
			resp.Body.Close()
			if decodeErr == nil {
				break
			}
			err = decodeErr
		}
		if i == maxRetries {
			fatal("Cannot reach orderer management API at %s after %d attempts: %v\n  Make sure the orderer node is running: ./kufichain run --data-dir .orderer", addr, maxRetries, err)
		}
		fmt.Printf("  ⏳ Attempt %d/%d failed (%v) — retrying in 10s...\n", i, maxRetries, err)
		time.Sleep(10 * time.Second)
	}
	fmt.Printf("  ✓ Connected to %s (%s)\n",
		bootstrapStatus["org_name"], bootstrapStatus["channel"])

	channelName := ""
	if ch, ok := bootstrapStatus["channel"].(string); ok {
		channelName = ch
	}
	netDomain := networkDomain
	if nd, ok := bootstrapStatus["network_domain"].(string); ok && nd != "" {
		netDomain = nd
	}

	// Gather org details — minimal input
	fmt.Println()
	orgName := ""
	externalHost := ""
	if opts != nil {
		orgName = strings.TrimSpace(opts.OrgName)
		externalHost = strings.TrimSpace(opts.ExternalHost)
	}
	if orgName == "" {
		orgName = prompt(reader, "Your organization/bank name (e.g. Paypal)", "")
	}
	if orgName == "" {
		fatal("Organization name is required")
	}
	if externalHost == "" {
		externalHost = prompt(reader, "This node's public IP or hostname", "")
	}
	if externalHost == "" {
		fatal("Public IP/hostname is required")
	}

	// Auto-derived
	mspID := sanitizeMSP(orgName)
	domain := strings.ToLower(sanitizeAlphaNum(orgName)) + "." + netDomain

	// Ports
	peerPort := 7051
	couchDBPort := 5984
	gatewayPort := 8080
	opsPort := 9444
	mgmtPort := 9500
	if opts != nil {
		if opts.PeerPort > 0 {
			peerPort = opts.PeerPort
		}
		if opts.CouchDBPort > 0 {
			couchDBPort = opts.CouchDBPort
		}
		if opts.GatewayPort > 0 {
			gatewayPort = opts.GatewayPort
		}
		if opts.OpsPort > 0 {
			opsPort = opts.OpsPort
		}
		if opts.MgmtPort > 0 {
			mgmtPort = opts.MgmtPort
		}
	}
	if opts == nil {
		fmt.Println()
		fmt.Println("  Port configuration (press Enter for defaults):")
		peerPort = promptInt(reader, "Peer port", peerPort)
		couchDBPort = promptInt(reader, "CouchDB port", couchDBPort)
		gatewayPort = promptInt(reader, "Gateway HTTP port", gatewayPort)
		opsPort = promptInt(reader, "Operations/metrics port", opsPort)
		mgmtPort = promptInt(reader, "Management API port", mgmtPort)
	}
	validatePort := func(name string, port int) {
		if port <= 0 || port > 65535 {
			fatal("Invalid %s: %d", name, port)
		}
	}
	validatePort("peer port", peerPort)
	validatePort("couchdb port", couchDBPort)
	validatePort("gateway port", gatewayPort)
	validatePort("ops port", opsPort)
	validatePort("management API port", mgmtPort)

	deployDir := filepath.Join(dataDir, "deploy")
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		fatal("Create deploy dir: %v", err)
	}

	nodeCfg := &nodemgr.NodeConfig{
		Role:            nodemgr.RolePeer,
		OrgName:         orgName,
		MSPID:           mspID,
		Domain:          domain,
		NetworkDomain:   netDomain,
		ChannelName:     channelName,
		PeerPort:        peerPort,
		ChaincodePort:   peerPort + 1,
		CouchDBPort:     couchDBPort,
		GatewayPort:     gatewayPort,
		OpsPort:         opsPort,
		MgmtPort:        mgmtPort,
		OrdererAddr:     "",   // filled after approval
		OrdererMgmtAddr: addr, // bootstrap address is the orderer's mgmt API
		ExternalHost:    externalHost,
		DataDir:         dataDir,
		DeployDir:       deployDir,
	}

	// Confirm
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────┐")
	fmt.Printf("│  Org:       %-31s │\n", orgName)
	fmt.Printf("│  MSP ID:    %-31s │\n", mspID+" (auto)")
	fmt.Printf("│  Domain:    %-31s │\n", domain+" (auto)")
	fmt.Printf("│  Host:      %-31s │\n", externalHost)
	fmt.Printf("│  Peer:      %-31s │\n", fmt.Sprintf(":%d", peerPort))
	fmt.Printf("│  CouchDB:   %-31s │\n", fmt.Sprintf(":%d", couchDBPort))
	fmt.Printf("│  Gateway:   %-31s │\n", fmt.Sprintf(":%d", gatewayPort))
	fmt.Printf("│  Mgmt API:  %-31s │\n", fmt.Sprintf(":%d", mgmtPort))
	fmt.Printf("│  Bootstrap: %-31s │\n", addr)
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  All crypto keys and certs will be auto-generated.")
	if opts == nil || !opts.AssumeYes {
		confirm := prompt(reader, "\n  Proceed? (y/n)", "y")
		if strings.ToLower(confirm) != "y" {
			fmt.Println("Aborted.")
			return
		}
	}

	// Init store
	if err := store.Init(); err != nil {
		fatal("Init store: %v", err)
	}
	if err := store.SaveNodeConfig(nodeCfg); err != nil {
		fatal("Save node config: %v", err)
	}

	// Step 1: Generate crypto
	step("1/3", "Generating crypto material for "+orgName+"...")
	if err := fabricops.GenerateOrgCrypto(deployDir, orgName, domain); err != nil {
		fatal("Crypto generation: %v", err)
	}
	if err := fabricops.EnsureFabricConfig(deployDir); err != nil {
		fatal("Generate Fabric config: %v", err)
	}
	fmt.Println("  ✓ Crypto material auto-generated (keys, TLS certs, CA)")

	// Step 2: Generate org definition + MSP bundle
	step("2/3", "Generating org definition...")
	orgDefJSON, err := fabricops.GenerateOrgDefinitionJSON(
		deployDir, orgName, mspID, domain, netDomain,
		fmt.Sprintf("peer0.%s", domain), peerPort)
	if err != nil {
		fatal("Org definition: %v", err)
	}

	orgCryptoDir := filepath.Join(deployDir, "crypto-config", "peerOrganizations", domain)
	mspBundle, err := fabricops.PackageMSPDir(orgCryptoDir)
	if err != nil {
		fatal("Package MSP: %v", err)
	}
	fmt.Println("  ✓ Org definition generated")

	// Start management server to receive approval notification
	mgr := nodemgr.NewManager(nodeCfg, store)
	srv := nodemgr.NewServer(mgr, mgmtPort)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "Management API error: %v\n", err)
		}
	}()
	time.Sleep(500 * time.Millisecond)

	// Step 3: Send join request
	step("3/3", "Sending join request to network...")
	joinReq := nodemgr.JoinRequest{
		OrgName:       orgName,
		MSPID:         mspID,
		Domain:        domain,
		PeerHost:      externalHost,
		AnchorHost:    fmt.Sprintf("peer0.%s", domain),
		AnchorPort:    peerPort,
		PeerPort:      peerPort,
		MgmtAddr:      fmt.Sprintf("http://%s:%d", externalHost, mgmtPort),
		OrgDefinition: orgDefJSON,
		OrgMSPBundle:  mspBundle,
	}

	reqBody, err := json.Marshal(joinReq)
	if err != nil {
		fatal("Encode join request: %v", err)
	}
	resp2, err := http.Post(addr+"/api/join-request", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		fatal("Send join request: %v", err)
	}
	defer resp2.Body.Close()
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		fatal("Read join response: %v", err)
	}

	var joinResp map[string]interface{}
	if err := json.Unmarshal(body, &joinResp); err != nil {
		fatal("Decode join response: %v", err)
	}

	if errMsg, ok := joinResp["error"]; ok {
		fatal("Join request rejected: %v", errMsg)
	}

	reqID := ""
	if id, ok := joinResp["id"].(string); ok {
		reqID = id
	}
	totalPeers := 0
	if tp, ok := joinResp["total_peers"].(float64); ok {
		totalPeers = int(tp)
	}
	requiredVotes := 0
	if rv, ok := joinResp["required_votes"].(float64); ok {
		requiredVotes = int(rv)
	}

	if len(reqID) > 12 {
		fmt.Printf("  ✓ Join request submitted (ID: %s)\n", reqID[:12])
	} else {
		fmt.Printf("  ✓ Join request submitted (ID: %s)\n", reqID)
	}
	fmt.Printf("    Network has %d peers — need %d approvals\n", totalPeers, requiredVotes)
	fmt.Println()
	fmt.Println("  ⏳ Waiting for approval from existing peers...")
	fmt.Println("     (On the orderer node, run 'kufichain run' to approve)")
	fmt.Println()

	// Wait for approval
	var notif *nodemgr.ApprovalNotification
	select {
	case notif = <-mgr.NotifyCh:
		if !notif.Approved {
			fatal("Join request was REJECTED")
		}
	case <-time.After(30 * time.Minute):
		fatal("Timeout waiting for approval (30 minutes)")
	}

	fmt.Println("  ✓ APPROVED! Joining network...")

	// Save orderer info
	nodeCfg.OrdererAddr = notif.OrdererAddr
	nodeCfg.NetworkDomain = notif.NetworkDomain
	nodeCfg.OrdererMgmtAddr = notif.OrdererMgmtAddr
	if channelName == "" {
		nodeCfg.ChannelName = notif.ChannelName
	}
	if err := store.SaveNodeConfig(nodeCfg); err != nil {
		fatal("Save node config: %v", err)
	}

	// Seed peer list from the approval notification
	for _, p := range notif.ExistingPeers {
		if err := store.AddPeer(p); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: add peer %s: %v\n", p.MSPID, err)
		}
	}
	for _, bundle := range notif.ExistingBundles {
		if bundle.Bundle == "" {
			continue
		}
		targetDir := filepath.Join(deployDir, "crypto-config", "peerOrganizations")
		if err := fabricops.UnpackMSPBundle(bundle.Bundle, targetDir); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: unpack org bundle %s: %v\n", bundle.MSPID, err)
		}
	}

	// Save orderer TLS CA cert
	if notif.OrdererTLSCA != "" {
		ordererCADir := filepath.Join(deployDir, "crypto-config", "ordererOrganizations",
			notif.NetworkDomain, "orderers", "orderer."+notif.NetworkDomain, "msp", "tlscacerts")
		if err := os.MkdirAll(ordererCADir, 0o755); err != nil {
			fatal("Create orderer CA dir: %v", err)
		}
		caData, err := base64.StdEncoding.DecodeString(notif.OrdererTLSCA)
		if err != nil {
			fatal("Decode orderer TLS CA: %v", err)
		}
		if err := os.WriteFile(filepath.Join(ordererCADir, "tlsca."+notif.NetworkDomain+"-cert.pem"), caData, 0o644); err != nil {
			fatal("Write orderer TLS CA: %v", err)
		}

		ordererTLSDir := filepath.Join(deployDir, "crypto-config", "ordererOrganizations",
			notif.NetworkDomain, "orderers", "orderer."+notif.NetworkDomain, "tls")
		if err := os.MkdirAll(ordererTLSDir, 0o755); err != nil {
			fatal("Create orderer TLS dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(ordererTLSDir, "ca.crt"), caData, 0o644); err != nil {
			fatal("Write orderer TLS root cert: %v", err)
		}
	}

	// Map channel's static orderer hostname (orderer.<networkDomain>) to the
	// advertised orderer IP so peer containers can resolve it cross-host.
	extraHosts := map[string]string{}
	ordererIP, err := resolveOrdererAliasIP(notif.OrdererAddr, notif.OrdererHostIP)
	if err != nil {
		fatal("Resolve orderer host '%s': %v. Use a public IP for the orderer host when setting up the orderer node.", notif.OrdererAddr, err)
	}
	if ordererIP != "" {
		extraHosts["orderer."+notif.NetworkDomain] = ordererIP
	}

	// Generate docker-compose for peer only
	dcCfg := fabricops.DockerConfig{
		NetworkName:   defaultNet,
		NetworkDomain: notif.NetworkDomain,
		OrgName:       orgName,
		MSPID:         mspID,
		Domain:        domain,
		PeerPort:      peerPort,
		ChaincodePort: peerPort + 1,
		OpsPort:       opsPort,
		CouchDBPort:   couchDBPort,
		ExtraHosts:    extraHosts,
	}
	if err := fabricops.GeneratePeerCompose(deployDir, dcCfg); err != nil {
		fatal("Generate peer docker-compose: %v", err)
	}

	// Start containers
	fmt.Println("  Starting peer containers...")
	// Remove stale fixed-name containers from legacy versions.
	fabricops.CleanupLegacyNamedPeerContainers(domain, orgName)
	if running := fabricops.ContainersRunning(deployDir); len(running) > 0 {
		fmt.Printf("  ⚠ Existing containers found: %s\n", strings.Join(running, ", "))
		choice := "d"
		if opts == nil || !opts.AssumeYes {
			reader := bufio.NewReader(os.Stdin)
			choice = prompt(reader, "  (r)euse them or (d)elete and recreate?", "d")
		}
		if strings.ToLower(choice) == "r" {
			fmt.Println("  ✓ Reusing existing containers")
		} else {
			if err := fabricops.CleanupContainers(deployDir); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: cleanup existing containers: %v\n", err)
			}
			if err := fabricops.StartContainers(deployDir); err != nil {
				fatal("Start containers: %v", err)
			}
		}
	} else {
		if err := fabricops.StartContainers(deployDir); err != nil {
			fatal("Start containers: %v", err)
		}
	}

	chOps := &fabricops.ChannelOps{
		DeployDir:   deployDir,
		ChannelName: nodeCfg.ChannelName,
		OrdererAddr: nodeCfg.OrdererAddr,
		NetworkDom:  nodeCfg.NetworkDomain,
		OrgName:     orgName,
		MSPID:       mspID,
		Domain:      domain,
		PeerPort:    peerPort,
	}

	// Wait for peer process + CLI readiness
	fmt.Print("  Waiting for peer")
	if !waitForService(deployDir, "peer0."+domain, 45) {
		fatal("Peer failed to start. Check compose logs in %s for service peer0.%s", deployDir, domain)
	}
	fmt.Println(" running")
	if err := chOps.WaitForPeerReady(45); err != nil {
		fatal("Peer is not ready: %v", err)
	}
	fmt.Println("  ✓ Peer CLI is ready")

	// Join peer to channel
	if err := chOps.FetchAndJoinPeer(); err != nil {
		fatal("Join peer to channel: %v", err)
	}
	fmt.Print("  Syncing channel blocks")
	if syncHeight := chOps.WaitForPeerSync(1, 90); syncHeight >= 1 {
		fmt.Printf(" synced to block %d\n", syncHeight)
	} else {
		fmt.Println(" timeout (will continue; chaincode deployment will retry)")
	}

	// Auto-generate gateway config
	fmt.Println("  Generating gateway configuration...")
	if err := generateGatewayConfig(nodeCfg); err != nil {
		fatal("Generate gateway config: %v", err)
	}

	fmt.Println()
	fmt.Println("  ╔═══════════════════════════════════════════════════╗")
	fmt.Println("  ║  ✓ Successfully joined the network!               ║")
	fmt.Println("  ╚═══════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("  Starting node (chaincode + gateway)...")
	fmt.Println()

	// Shut down the temporary management server before startDashboard creates its own
	srv.Shutdown()

	startDashboard(store, nodeCfg)
}

// =====================================================================
// RUN — Start node: containers + gateway (peer) + interactive dashboard
// =====================================================================

func cmdRun() {
	dataDir := resolveDataDir()
	store := nodemgr.NewStore(dataDir)

	if !store.NodeConfigExists() {
		fmt.Println("Node not initialized.")
		fmt.Println()
		fmt.Println("  To set up a new node:        ./kufichain setup")
		fmt.Println("  To join existing network:    ./kufichain join --bootstrap <host:port>")
		os.Exit(1)
	}

	nodeCfg, err := store.LoadNodeConfig()
	if err != nil {
		fatal("Load config: %v", err)
	}

	startDashboard(store, nodeCfg)
}

func startDashboard(store *nodemgr.Store, nodeCfg *nodemgr.NodeConfig) {
	isPeer := nodeCfg.Role != nodemgr.RoleOrderer

	if isPeer {
		printBanner("KufiChain — Starting " + nodeCfg.OrgName)
	} else {
		printBanner("KufiChain — Orderer Dashboard")
	}

	// Ensure containers are running (they may have been stopped)
	fmt.Println("  Checking containers...")
	if err := fabricops.StartContainers(nodeCfg.DeployDir); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not start containers: %v\n", err)
	} else {
		fmt.Println("  ✓ Containers running")
	}

	// Start management API early so this node can participate in voting/gossip
	// even while channel sync and chaincode deployment are still in progress.
	mgr := nodemgr.NewManager(nodeCfg, store)
	srv := nodemgr.NewServer(mgr, nodeCfg.MgmtPort)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "Management API error: %v\n", err)
		}
	}()

	// For peer nodes: sync peer list from orderer on startup
	if isPeer && nodeCfg.OrdererMgmtAddr != "" {
		syncPeersFromOrderer(store, nodeCfg.OrdererMgmtAddr)
	}

	// Start heartbeat goroutine — sends heartbeat to all gossip targets every 30s
	go startHeartbeat(mgr, nodeCfg)

	// For orderer nodes: verify the channel exists (it could be lost if the Docker volume was recreated)
	if !isPeer {
		containerName := "orderer." + nodeCfg.NetworkDomain
		fmt.Print("  Waiting for orderer")
		if !waitForService(nodeCfg.DeployDir, containerName, 15) {
			fmt.Fprintf(os.Stderr, "\n  Warning: orderer container not running\n")
		} else {
			fmt.Println(" running")
		}

		chOps := &fabricops.ChannelOps{
			DeployDir:      nodeCfg.DeployDir,
			ChannelName:    nodeCfg.ChannelName,
			OrdererAddr:    nodeCfg.OrdererAddr,
			NetworkDom:     nodeCfg.NetworkDomain,
			OrgName:        "Orderer",
			MSPID:          "OrdererMSP",
			Domain:         nodeCfg.NetworkDomain,
			AdminPort:      nodeCfg.OrdererAdminPort,
			IsOrdererAdmin: true,
		}
		if err := chOps.WaitForOrdererAdminReady(30); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: orderer admin API not ready: %v\n", err)
		}
		if !chOps.VerifyChannelExists() {
			fmt.Printf("  ⚠ Channel '%s' not found on orderer — recreating...\n", nodeCfg.ChannelName)
			if err := chOps.JoinOrdererToChannel(); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: rejoin channel failed: %v\n", err)
			} else {
				fmt.Printf("  ✓ Channel '%s' restored\n", nodeCfg.ChannelName)
			}
		} else {
			fmt.Printf("  ✓ Channel '%s' exists\n", nodeCfg.ChannelName)
		}
	}

	// For peer nodes: ensure channel joined, then deploy chaincode
	var ccOps *fabricops.ChaincodeOps
	if isPeer {
		containerName := "peer0." + nodeCfg.Domain
		fmt.Print("  Waiting for peer")
		if !waitForService(nodeCfg.DeployDir, containerName, 20) {
			fmt.Fprintf(os.Stderr, "\n  Warning: peer container not running\n")
		} else {
			fmt.Println(" running")
		}

		// Ensure peer has joined the channel (auto-rejoin if volumes were recreated)
		chOps := &fabricops.ChannelOps{
			DeployDir:   nodeCfg.DeployDir,
			ChannelName: nodeCfg.ChannelName,
			OrdererAddr: nodeCfg.OrdererAddr,
			NetworkDom:  nodeCfg.NetworkDomain,
			OrgName:     nodeCfg.OrgName,
			MSPID:       nodeCfg.MSPID,
			Domain:      nodeCfg.Domain,
			PeerPort:    nodeCfg.PeerPort,
		}
		if err := chOps.WaitForPeerReady(45); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: peer CLI not ready: %v\n", err)
		}
		if !chOps.PeerHasJoinedChannel() {
			fmt.Printf("  Channel '%s' not joined — rejoining...\n", nodeCfg.ChannelName)
			if err := chOps.FetchAndJoinPeer(); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: rejoin channel: %v\n", err)
			} else {
				fmt.Printf("  ✓ Joined channel '%s'\n", nodeCfg.ChannelName)
			}
		} else {
			fmt.Printf("  ✓ Channel '%s' joined\n", nodeCfg.ChannelName)
		}
		// Wait for peer to sync config blocks from orderer. The genesis block
		// alone is not enough for lifecycle commands in dynamic-org networks.
		fmt.Print("  Syncing channel blocks")
		syncHeight := chOps.WaitForPeerSync(1, 90)
		if syncHeight >= 1 {
			fmt.Printf(" synced to block %d\n", syncHeight)
		} else {
			fmt.Println(" timeout (chaincode deployment will keep retrying)")
		}

		ccOps = &fabricops.ChaincodeOps{
			DeployDir:       nodeCfg.DeployDir,
			ChannelName:     nodeCfg.ChannelName,
			OrdererAddr:     nodeCfg.OrdererAddr,
			NetworkDom:      nodeCfg.NetworkDomain,
			OrgName:         nodeCfg.OrgName,
			MSPID:           nodeCfg.MSPID,
			Domain:          nodeCfg.Domain,
			PeerPort:        nodeCfg.PeerPort,
			ChaincodeName:   "payment",
			ChaincodeVer:    "1.0",
			CCLabel:         "payment_1.0",
			ProjectRoot:     findProjectRoot(),
			PeerEndpoints:   buildPeerEndpoints(store, nodeCfg),
			SignaturePolicy: buildSignaturePolicy(store, nodeCfg),
		}

		fmt.Println("  Chaincode: ensuring deployment...")
		if err := ccOps.EnsureChaincodeDeployed(); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: chaincode deployment: %v\n", err)
		}
	}

	// For peer nodes: periodically try to commit chaincode if all orgs have approved.
	// Recompute the OR policy each tick so new peers are picked up automatically.
	if isPeer && ccOps != nil {
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				ccOps.PeerEndpoints = buildPeerEndpoints(store, nodeCfg)
				newPolicy := buildSignaturePolicy(store, nodeCfg)
				if newPolicy != "" {
					ccOps.SignaturePolicy = newPolicy
				}
				if ccOps.TryCommitIfReady() {
					fmt.Println("\n  ✓ Chaincode committed to channel — fully operational!")
					return // stop checking once committed
				}
			}
		}()
	}

	// For peer nodes: start the payment gateway in the background
	var gwCmd *exec.Cmd
	if isPeer {
		dataDir := nodeCfg.DataDir
		gwConfigPath := filepath.Join(dataDir, "gateway.yaml")
		if _, err := os.Stat(gwConfigPath); os.IsNotExist(err) {
			fmt.Println("  Gateway config not found. Generating...")
			if err := generateGatewayConfig(nodeCfg); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: generate gateway config: %v\n", err)
			}
		}

		gatewayBin := filepath.Join(findProjectRoot(), "gateway")
		if _, err := os.Stat(gatewayBin); os.IsNotExist(err) {
			fmt.Println("  Gateway binary not found. Building...")
			projectDir := findProjectRoot()
			buildCmd := exec.Command("go", "build", "-o", gatewayBin, "./cmd/gateway")
			buildCmd.Dir = projectDir
			buildCmd.Stdout = os.Stdout
			buildCmd.Stderr = os.Stderr
			if err := buildCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: build gateway: %v\n", err)
			}
		}

		if _, err := os.Stat(gatewayBin); err == nil {
			gwCmd = exec.Command(gatewayBin, "-config", gwConfigPath)
			gwCmd.Dir = findProjectRoot()
			// Gateway logs go to a file so they don't pollute the dashboard
			logPath := filepath.Join(dataDir, "gateway.log")
			logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if logFile != nil {
				gwCmd.Stdout = logFile
				gwCmd.Stderr = logFile
			}
			if err := gwCmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: start gateway: %v\n", err)
				gwCmd = nil
			} else {
				gwPort := nodeCfg.GatewayPort
				if gwPort == 0 {
					gwPort = 8080
				}
				fmt.Printf("  ✓ Gateway started on :%d (logs: %s)\n", gwPort, logPath)
			}
		}
	}

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Async stdin reader — all line reads go through this channel.
	// If stdin is closed (e.g. nohup / headless mode), we block forever
	// so the dashboard stays running and only exits on SIGTERM/SIGINT.
	inputCh := make(chan string, 10)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				// stdin closed (nohup / piped) — run headless indefinitely
				select {} // blocks until process is killed
			}
			inputCh <- strings.TrimSpace(line)
		}
	}()

	// Main event loop — re-renders dashboard on user input, events, or signals
	for {
		renderDashboard(store, nodeCfg)
		fmt.Print("\n> ")

		select {
		case input, ok := <-inputCh:
			if !ok {
				return
			}
			if input == "" {
				continue
			}

			parts := strings.Fields(input)
			cmd := parts[0]

			switch cmd {
			case "a", "approve":
				if len(parts) < 2 {
					fmt.Println("  Usage: a <request-number>")
					continue
				}
				handleVoteCmd(mgr, store, parts[1], true, inputCh)
			case "r", "reject":
				if len(parts) < 2 {
					fmt.Println("  Usage: r <request-number>")
					continue
				}
				handleVoteCmd(mgr, store, parts[1], false, inputCh)
			case "p", "peers":
				showPeers(store)
				fmt.Print("\n  Press Enter to continue...")
				<-inputCh
			case "s", "status":
				showDetailedStatus(store, nodeCfg)
				fmt.Print("\n  Press Enter to continue...")
				<-inputCh
			case "l", "list":
				showAllRequests(store)
				fmt.Print("\n  Press Enter to continue...")
				<-inputCh
			case "log", "logs":
				if isPeer {
					logPath := filepath.Join(nodeCfg.DataDir, "gateway.log")
					data, err := os.ReadFile(logPath)
					if err != nil {
						fmt.Printf("  Cannot read log: %v\n", err)
					} else {
						lines := strings.Split(string(data), "\n")
						start := 0
						if len(lines) > 20 {
							start = len(lines) - 20
						}
						fmt.Println("  --- Last 20 lines of gateway.log ---")
						for _, l := range lines[start:] {
							fmt.Println("  " + l)
						}
					}
				} else {
					fmt.Println("  No gateway on orderer nodes")
				}
				fmt.Print("\n  Press Enter to continue...")
				<-inputCh
			case "q", "quit":
				if gwCmd != nil && gwCmd.Process != nil {
					fmt.Println("  Gateway + management API will keep running in background.")
				} else {
					fmt.Println("  Management API will keep running in background.")
				}
				fmt.Println("  Run 'kufichain run' to reconnect.")
				return
			case "stop":
				if gwCmd != nil && gwCmd.Process != nil {
					fmt.Println("  Stopping gateway...")
					gwCmd.Process.Signal(syscall.SIGTERM)
					gwCmd.Wait()
				}
				if ccOps != nil {
					fmt.Println("  Stopping chaincode container...")
					ccOps.StopCCContainer()
				}
				fmt.Println("  Stopping containers...")
				fabricops.StopContainers(nodeCfg.DeployDir)
				srv.Shutdown()
				fmt.Println("  ✓ Stopped")
				return
			default:
				fmt.Printf("  Unknown command: %s\n", cmd)
				if isPeer {
					fmt.Println("  Commands: a(pprove) r(eject) p(eers) s(tatus) l(ist) log(s) q(uit) stop")
				} else {
					fmt.Println("  Commands: a(pprove) r(eject) p(eers) s(tatus) l(ist) q(uit) stop")
				}
			}

		case <-mgr.RefreshCh:
			// Drain any extra refresh signals
			drainRefreshCh(mgr.RefreshCh)
			// Re-evaluate chaincode policy when something changes (e.g. new peer joined)
			if isPeer && ccOps != nil {
				ccOps.PeerEndpoints = buildPeerEndpoints(store, nodeCfg)
				newPolicy := buildSignaturePolicy(store, nodeCfg)
				if newPolicy != "" && newPolicy != ccOps.SignaturePolicy {
					ccOps.SignaturePolicy = newPolicy
					go func() {
						if ccOps.TryCommitIfReady() {
							fmt.Println("\n  ✓ Chaincode OR-policy upgrade committed!")
						}
					}()
				}
			}
			// Re-render on next loop iteration

		case <-sigCh:
			fmt.Println("\n  Shutting down...")
			if gwCmd != nil && gwCmd.Process != nil {
				gwCmd.Process.Signal(syscall.SIGTERM)
			}
			if ccOps != nil {
				ccOps.StopCCContainer()
			}
			srv.Shutdown()
			return
		}
	}
}

// =====================================================================
// STATUS / STOP
// =====================================================================

func cmdStatus() {
	dataDir := resolveDataDir()
	store := nodemgr.NewStore(dataDir)

	if !store.NodeConfigExists() {
		fmt.Println("Node not initialized.")
		return
	}

	cfg, _ := store.LoadNodeConfig()
	showDetailedStatus(store, cfg)
}

func cmdStop() {
	dataDir := resolveDataDir()
	store := nodemgr.NewStore(dataDir)

	if !store.NodeConfigExists() {
		fmt.Println("Node not initialized.")
		return
	}

	cfg, _ := store.LoadNodeConfig()

	// Stop chaincode container if this is a peer node
	if cfg.Role != nodemgr.RoleOrderer {
		ccOps := &fabricops.ChaincodeOps{
			OrgName:       cfg.OrgName,
			ChaincodeName: "payment",
			ChaincodeVer:  "1.0",
		}
		fmt.Println("Stopping chaincode container...")
		ccOps.StopCCContainer()
	}

	fmt.Println("Stopping containers...")
	if err := fabricops.StopContainers(cfg.DeployDir); err != nil {
		fatal("Stop: %v", err)
	}
	fmt.Println("✓ Stopped")
}

// =====================================================================
// Gateway config auto-generation
// =====================================================================

func generateGatewayConfig(cfg *nodemgr.NodeConfig) error {
	deployDir := cfg.DeployDir

	// Find cert and key paths
	certPath := filepath.Join(deployDir, "crypto-config", "peerOrganizations", cfg.Domain,
		"users", fmt.Sprintf("Admin@%s", cfg.Domain), "msp", "signcerts", "cert.pem")
	// Key may have a dynamic name — find it
	keyDir := filepath.Join(deployDir, "crypto-config", "peerOrganizations", cfg.Domain,
		"users", fmt.Sprintf("Admin@%s", cfg.Domain), "msp", "keystore")
	keyPath := ""
	entries, err := os.ReadDir(keyDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), "_sk") {
				keyPath = filepath.Join(keyDir, e.Name())
				break
			}
		}
	}
	if keyPath == "" {
		keyPath = filepath.Join(keyDir, "priv_sk")
	}

	// Find cert file — might be named Admin@domain-cert.pem in signcerts
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		signcertsDir := filepath.Dir(certPath)
		entries, err := os.ReadDir(signcertsDir)
		if err == nil && len(entries) > 0 {
			certPath = filepath.Join(signcertsDir, entries[0].Name())
		}
	}

	// TLS root cert for the peer
	tlsRootCert := filepath.Join(deployDir, "crypto-config", "peerOrganizations", cfg.Domain,
		"peers", fmt.Sprintf("peer0.%s", cfg.Domain), "tls", "ca.crt")

	// Generate YAML
	gatewayPort := cfg.GatewayPort
	if gatewayPort == 0 {
		gatewayPort = 8080
	}
	yaml := fmt.Sprintf(`# Auto-generated by kufichain — do not edit manually
server:
  host: "0.0.0.0"
  port: %d
  read_timeout: 30s
  write_timeout: 30s
  shutdown_timeout: 10s

fabric:
  msp_id: "%s"
  cert_path: "%s"
  key_path: "%s"
  tls:
    enabled: true
    root_cert_path: "%s"
  peers:
    - name: "peer0.%s"
      endpoint: "localhost:%d"
      tls_cert_path: "%s"
      override_authority: "peer0.%s"
  channel_name: "%s"
  chaincode:
    name: "payment"
    version: "1.0"
  timeouts:
    connect: 10s
    endorse: 30s
    submit: 30s
    commit_status: 60s
    evaluate: 10s
  max_concurrent: 64

transaction:
  idempotency_ttl: 24h
  nonce_window: 5m
  high_risk_threshold: 1000000000
  rate_limit: 100
  rate_limit_window: 1m

worker_pool:
  size: 16
  queue_size: 256

logging:
  level: "info"
  format: "json"
  output: "stdout"

data_dir: "%s"
`,
		gatewayPort,
		cfg.MSPID,
		certPath,
		keyPath,
		tlsRootCert,
		cfg.Domain, cfg.PeerPort,
		tlsRootCert,
		cfg.Domain,
		cfg.ChannelName,
		filepath.Join(cfg.DataDir, "gateway-data"),
	)

	gwConfigPath := filepath.Join(cfg.DataDir, "gateway.yaml")
	os.MkdirAll(filepath.Dir(gwConfigPath), 0o755)
	return os.WriteFile(gwConfigPath, []byte(yaml), 0o644)
}

// =====================================================================
// UI Helpers
// =====================================================================

func renderDashboard(store *nodemgr.Store, cfg *nodemgr.NodeConfig) {
	peers, _ := store.LoadPeers()
	pending, _ := store.ListJoinRequests(nodemgr.StatusPending)

	online := 0
	for _, p := range peers {
		if !p.LastSeen.IsZero() && time.Since(p.LastSeen) < 90*time.Second {
			online++
		}
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Printf("║  KufiChain — %-35s║\n", cfg.OrgName)
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf("║  Role:    %-39s ║\n", string(cfg.Role))
	fmt.Printf("║  Channel: %-39s ║\n", cfg.ChannelName)
	if cfg.Role == nodemgr.RoleOrderer {
		port := cfg.OrdererPort
		if port == 0 {
			port = 7050
		}
		fmt.Printf("║  Orderer: %-39s ║\n", fmt.Sprintf(":%d", port))
	} else {
		fmt.Printf("║  Peer:    %-39s ║\n", fmt.Sprintf(":%d", cfg.PeerPort))
	}
	fmt.Printf("║  Mgmt:    %-39s ║\n", fmt.Sprintf(":%d", cfg.MgmtPort))
	if cfg.Role != nodemgr.RoleOrderer {
		gwPort := cfg.GatewayPort
		if gwPort == 0 {
			gwPort = 8080
		}
		fmt.Printf("║  Gateway: %-39s ║\n", fmt.Sprintf(":%d", gwPort))
	}
	peerStatus := fmt.Sprintf("%d total, %d online", len(peers), online)
	fmt.Printf("║  Peers:   %-39s ║\n", peerStatus)
	fmt.Println("╠══════════════════════════════════════════════════╣")

	if len(pending) > 0 {
		fmt.Println("║  PENDING JOIN REQUESTS:                          ║")
		for i, req := range pending {
			votes := fmt.Sprintf("%d/%d votes", req.ApprovalCount(), req.RequiredVotes())
			line := fmt.Sprintf("  [%d] %s (%s) — %s", i+1, req.OrgName, req.MSPID, votes)
			fmt.Printf("║  %-48s║\n", line)
		}
	} else {
		fmt.Println("║  No pending join requests                        ║")
	}
	fmt.Println("╠══════════════════════════════════════════════════╣")
	if cfg.Role != nodemgr.RoleOrderer {
		fmt.Println("║  a <n> Approve | r <n> Reject | p Peers          ║")
		fmt.Println("║  s Status | l List | log Gateway logs | q Quit   ║")
	} else {
		fmt.Println("║  a <n> Approve | r <n> Reject | p Peers          ║")
		fmt.Println("║  s Status | l List all | q Quit | stop           ║")
	}
	fmt.Println("╚══════════════════════════════════════════════════╝")
}

func handleVoteCmd(mgr *nodemgr.Manager, store *nodemgr.Store, numStr string, approve bool, inputCh <-chan string) {
	num, err := strconv.Atoi(numStr)
	if err != nil {
		fmt.Println("  Invalid number")
		return
	}

	pending, _ := store.ListJoinRequests(nodemgr.StatusPending)
	if num < 1 || num > len(pending) {
		fmt.Printf("  No request #%d\n", num)
		return
	}

	req := pending[num-1]
	action := "Approve"
	if !approve {
		action = "Reject"
	}
	fmt.Printf("  %s %s (%s)? (y/n): ", action, req.OrgName, req.MSPID)
	confirm, ok := <-inputCh
	if !ok {
		return
	}
	if strings.ToLower(confirm) != "y" {
		fmt.Println("  Cancelled")
		return
	}

	if err := mgr.CastVote(req.ID, approve); err != nil {
		fmt.Printf("  Error: %v\n", err)
		return
	}

	if approve {
		fmt.Printf("  ✓ Approved %s\n", req.OrgName)
	} else {
		fmt.Printf("  ✗ Rejected %s\n", req.OrgName)
	}
}

func showPeers(store *nodemgr.Store) {
	peers, _ := store.LoadPeers()
	if len(peers) == 0 {
		fmt.Println("  No peers")
		return
	}
	fmt.Println("  Known peers:")
	for _, p := range peers {
		status := "offline"
		if !p.LastSeen.IsZero() && time.Since(p.LastSeen) < 90*time.Second {
			status = "online"
		}
		fmt.Printf("    %-15s %-20s [%s]  peer=%s  mgmt=%s\n",
			p.OrgName, p.MSPID, status, p.PeerAddr, p.MgmtAddr)
	}
}

func showDetailedStatus(store *nodemgr.Store, cfg *nodemgr.NodeConfig) {
	fmt.Printf("  Role:       %s\n", cfg.Role)
	fmt.Printf("  Org:        %s (%s)\n", cfg.OrgName, cfg.MSPID)
	fmt.Printf("  Domain:     %s\n", cfg.Domain)
	fmt.Printf("  Channel:    %s\n", cfg.ChannelName)
	if cfg.Role == nodemgr.RoleOrderer {
		port := cfg.OrdererPort
		if port == 0 {
			port = 7050
		}
		fmt.Printf("  Orderer:    :%d\n", port)
	} else {
		fmt.Printf("  Peer:       :%d\n", cfg.PeerPort)
		fmt.Printf("  CouchDB:    :%d\n", cfg.CouchDBPort)
	}
	fmt.Printf("  Mgmt:       :%d\n", cfg.MgmtPort)
	fmt.Printf("  Orderer:    %s\n", cfg.OrdererAddr)
	fmt.Printf("  External:   %s\n", cfg.ExternalHost)
	fmt.Printf("  Deploy dir: %s\n", cfg.DeployDir)

	status, err := fabricops.ContainerStatus(cfg.DeployDir)
	if err == nil {
		fmt.Println("\n  Container status:")
		fmt.Println("  " + strings.ReplaceAll(status, "\n", "\n  "))
	}
}

func showAllRequests(store *nodemgr.Store) {
	requests, _ := store.ListJoinRequests("")
	if len(requests) == 0 {
		fmt.Println("  No requests")
		return
	}
	for _, req := range requests {
		fmt.Printf("  [%s] %s (%s) — status=%s votes=%d/%d\n",
			req.ID[:8], req.OrgName, req.MSPID,
			req.Status, req.ApprovalCount(), req.RequiredVotes())
	}
}

// =====================================================================
// Heartbeat & Peer Sync
// =====================================================================

// syncPeersFromOrderer fetches the orderer's peer list and merges into local store.
func syncPeersFromOrderer(store *nodemgr.Store, ordererMgmtAddr string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ordererMgmtAddr + "/api/peers")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var peers []nodemgr.PeerInfo
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return
	}
	for _, p := range peers {
		store.AddPeer(p)
	}
}

// startHeartbeat periodically sends a heartbeat to all gossip targets.
func startHeartbeat(mgr *nodemgr.Manager, cfg *nodemgr.NodeConfig) {
	hb := &nodemgr.Heartbeat{
		MSPID:    cfg.MSPID,
		OrgName:  cfg.OrgName,
		MgmtAddr: fmt.Sprintf("http://%s:%d", cfg.ExternalHost, cfg.MgmtPort),
	}

	// Send immediately
	mgr.Gossip.SendHeartbeat(hb)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		mgr.Gossip.SendHeartbeat(hb)
	}
}

// drainRefreshCh empties any pending signals from the refresh channel.
func drainRefreshCh(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// =====================================================================
// Helpers
// =====================================================================

// buildPeerEndpoints returns the current peer topology for lifecycle commit.
// The topology comes from the replicated peer registry instead of local
// sibling directories, so it works across multiple machines.
func buildPeerEndpoints(store *nodemgr.Store, cfg *nodemgr.NodeConfig) []fabricops.PeerEndpoint {
	peers := collectKnownPeers(store, cfg)
	endpoints := make([]fabricops.PeerEndpoint, 0, len(peers))
	for _, peer := range peers {
		if peer.Domain == "" || peer.PeerAddr == "" {
			continue
		}
		tlsCert := filepath.Join(cfg.DeployDir, "crypto-config", "peerOrganizations", peer.Domain,
			"peers", fmt.Sprintf("peer0.%s", peer.Domain), "tls", "ca.crt")
		if _, err := os.Stat(tlsCert); err != nil {
			continue
		}
		endpoints = append(endpoints, fabricops.PeerEndpoint{
			Addr:        peer.PeerAddr,
			TLSCertPath: tlsCert,
			MgmtAddr:    peer.MgmtAddr,
		})
	}
	return endpoints
}

// buildSignaturePolicy constructs an OR endorsement policy covering all known
// peer organizations so any bank/org node can endorse after it joins.
func buildSignaturePolicy(store *nodemgr.Store, cfg *nodemgr.NodeConfig) string {
	peers := collectKnownPeers(store, cfg)
	mspIDs := make([]string, 0, len(peers))
	seen := make(map[string]struct{}, len(peers))
	for _, peer := range peers {
		if peer.MSPID == "" {
			continue
		}
		if _, ok := seen[peer.MSPID]; ok {
			continue
		}
		seen[peer.MSPID] = struct{}{}
		mspIDs = append(mspIDs, peer.MSPID)
	}
	sort.Strings(mspIDs)
	parts := make([]string, len(mspIDs))
	for i, id := range mspIDs {
		parts[i] = fmt.Sprintf("'%s.peer'", id)
	}
	return fmt.Sprintf("OR(%s)", strings.Join(parts, ","))
}

func collectKnownPeers(store *nodemgr.Store, cfg *nodemgr.NodeConfig) []nodemgr.PeerInfo {
	peersByMSP := make(map[string]nodemgr.PeerInfo)
	if store != nil {
		if peers, err := store.LoadPeers(); err == nil {
			for _, peer := range peers {
				if peer.MSPID == "" {
					continue
				}
				peersByMSP[peer.MSPID] = peer
			}
		}
	}
	if cfg != nil && cfg.Role == nodemgr.RolePeer && cfg.MSPID != "" {
		host := strings.TrimSpace(cfg.ExternalHost)
		if host == "" {
			host = fmt.Sprintf("peer0.%s", cfg.Domain)
		}
		peersByMSP[cfg.MSPID] = nodemgr.PeerInfo{
			OrgName:  cfg.OrgName,
			MSPID:    cfg.MSPID,
			Domain:   cfg.Domain,
			PeerAddr: net.JoinHostPort(host, strconv.Itoa(cfg.PeerPort)),
			MgmtAddr: fmt.Sprintf("http://%s:%d", host, cfg.MgmtPort),
		}
	}

	peers := make([]nodemgr.PeerInfo, 0, len(peersByMSP))
	for _, peer := range peersByMSP {
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].MSPID < peers[j].MSPID
	})
	return peers
}

// resolveOrdererAliasIP resolves the IP used to map orderer.<networkDomain>
// in peer containers. The hinted IP from approval notification is preferred.
func resolveOrdererAliasIP(ordererAddr, hintedIP string) (string, error) {
	if ip := net.ParseIP(strings.TrimSpace(hintedIP)); ip != nil {
		// Never return loopback addresses for container host mapping.
		// 127.0.0.1 inside a peer container points to itself.
		if !ip.IsLoopback() {
			return ip.String(), nil
		}
	}
	host := strings.TrimSpace(ordererAddr)
	if h, _, err := net.SplitHostPort(ordererAddr); err == nil {
		host = h
	} else if strings.Count(ordererAddr, ":") > 1 {
		// Best-effort IPv6-without-port handling.
		host = strings.Trim(ordererAddr, "[]")
	}
	if host == "" || host == "localhost" || host == "127.0.0.1" {
		return "", nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no A/AAAA record for %s", host)
}

func findProjectRoot() string {
	// Try executable directory first (handles CWD != project root)
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		if realExe, err := filepath.EvalSymlinks(exe); err == nil {
			exeDir = filepath.Dir(realExe)
		}
		if _, err := os.Stat(filepath.Join(exeDir, "go.mod")); err == nil {
			return exeDir
		}
	}
	// Fall back to CWD-based search
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	d, _ := os.Getwd()
	return d
}

func prompt(reader *bufio.Reader, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("  %s [%s]: ", label, defaultVal)
	} else {
		fmt.Printf("  %s: ", label)
	}
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

func promptInt(reader *bufio.Reader, label string, defaultVal int) int {
	s := prompt(reader, label, strconv.Itoa(defaultVal))
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

func printBanner(title string) {
	w := len(title) + 6
	border := strings.Repeat("═", w)
	fmt.Println()
	fmt.Printf("╔%s╗\n", border)
	fmt.Printf("║   %s   ║\n", title)
	fmt.Printf("╚%s╝\n", border)
	fmt.Println()
}

func step(num, msg string) {
	fmt.Printf("\n[%s] %s\n", num, msg)
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "\n  ERROR: "+format+"\n\n", args...)
	os.Exit(1)
}

func waitForService(deployDir, service string, maxSeconds int) bool {
	for i := 0; i < maxSeconds; i++ {
		time.Sleep(time.Second)
		fmt.Print(".")
		if err := fabricops.WaitForService(deployDir, service, 1); err == nil {
			return true
		}
	}
	fmt.Println()
	return false
}

// sanitizeMSP converts an org name to a valid MSP ID: "SC Bank" → "SCBankMSP"
func sanitizeMSP(name string) string {
	// Remove spaces and special chars, capitalize first letter of each word
	words := strings.Fields(name)
	var parts []string
	for _, w := range words {
		if len(w) > 0 {
			parts = append(parts, strings.ToUpper(w[:1])+w[1:])
		}
	}
	id := strings.Join(parts, "")
	// Remove any non-alphanumeric
	var clean []byte
	for _, c := range []byte(id) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			clean = append(clean, c)
		}
	}
	return string(clean) + "MSP"
}

// sanitizeAlphaNum removes non-alphanumeric chars
func sanitizeAlphaNum(name string) string {
	var clean []byte
	for _, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			clean = append(clean, c)
		}
	}
	return string(clean)
}
