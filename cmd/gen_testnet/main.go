// gen_testnet generates a multi-node testnet configuration for SupernovaCore.
// Must be built with: go build -tags bls12381 -o bin/gen_testnet ./cmd/gen_testnet
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cometbft/cometbft/v2/crypto/bls12381"
	"github.com/cometbft/cometbft/v2/crypto/ed25519"
	"github.com/cometbft/cometbft/v2/privval"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	"github.com/meterio/supernova/types"
)

func main() {
	var (
		numNodes = flag.Int("nodes", 4, "number of validator nodes to generate")
		outDir   = flag.String("out-dir", "./mytestnet", "output directory for testnet configs")
		chainID  = flag.String("chain-id", "100", "chain ID (must be numeric for SupernovaCore)")
		force    = flag.Bool("force", false, "force regeneration even if output directory already exists")
	)
	flag.Parse()

	// Check if already initialized
	node0Config := filepath.Join(*outDir, "node0", "config")
	if !*force {
		if _, err := os.Stat(node0Config); err == nil {
			fmt.Printf("✅ Testnet already initialized at %s (use --force to regenerate)\n", *outDir)
			// Still print the existing nodes.json if present
			nodesFile := filepath.Join(*outDir, "nodes.json")
			if data, err := os.ReadFile(nodesFile); err == nil {
				fmt.Println(string(data))
			}
			return
		}
	}

	fmt.Printf("🔧 Generating %d-node testnet in %s (chain-id: %s)...\n\n", *numNodes, *outDir, *chainID)

	// Port layout: each node gets a block of 100 ports
	// Node i: P2P = 26656 + i*100, RPC = 26657 + i*100, libp2p TCP = P2P+1000, libp2p UDP = P2P+2000
	p2pPorts := make([]int, *numNodes)
	rpcPorts := make([]int, *numNodes)
	for i := 0; i < *numNodes; i++ {
		p2pPorts[i] = 26656 + i*100
		rpcPorts[i] = 26657 + i*100
	}

	type NodeInfo struct {
		nodeID string
		addr   string
	}

	nodeInfos := make([]NodeInfo, *numNodes)
	privVals := make([]*privval.FilePV, *numNodes)

	// ── Step 1: Create directories and generate keys ──────────────────────────
	for i := 0; i < *numNodes; i++ {
		nodeDir := filepath.Join(*outDir, fmt.Sprintf("node%d", i))
		configDir := filepath.Join(nodeDir, "config")
		dataDir := filepath.Join(nodeDir, "data")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			fatalf("create config dir: %v", err)
		}
		if err := os.MkdirAll(dataDir, 0755); err != nil {
			fatalf("create data dir: %v", err)
		}

		// ed25519 node key — used for P2P identity
		edPrivKey := ed25519.GenPrivKey()
		nk := &types.NodeKey{PrivKey: edPrivKey}
		nodeKeyFile := filepath.Join(configDir, "node_key.json")
		if err := nk.SaveAs(nodeKeyFile); err != nil {
			fatalf("save node key: %v", err)
		}
		nodeInfos[i].nodeID = string(nk.ID())

		// BLS12-381 validator key — used for consensus signing
		blsPrivKey, err := bls12381.GenPrivKey()
		if err != nil {
			fatalf("gen BLS key: %v", err)
		}
		pvKeyFile := filepath.Join(configDir, "priv_validator_key.json")
		pvStateFile := filepath.Join(dataDir, "priv_validator_state.json")
		pv := privval.NewFilePV(blsPrivKey, pvKeyFile, pvStateFile)
		pv.Save()
		privVals[i] = pv

		nodeInfos[i].addr = blsPrivKey.PubKey().Address().String()
		fmt.Printf("  node%d  node_id=%s  validator_addr=%s\n", i, nodeInfos[i].nodeID, nodeInfos[i].addr)
	}

	// ── Step 2: Build genesis doc ──────────────────────────────────────────────
	var genesisValidators []cmttypes.GenesisValidator
	for i := 0; i < *numNodes; i++ {
		pubKey, err := privVals[i].GetPubKey()
		if err != nil {
			fatalf("get pubkey for node%d: %v", i, err)
		}
		genesisValidators = append(genesisValidators, cmttypes.GenesisValidator{
			Address: pubKey.Address(),
			PubKey:  pubKey,
			Power:   10,
			Name:    fmt.Sprintf("node%d", i),
		})
	}

	consParams := cmttypes.DefaultConsensusParams()
	consParams.Validator.PubKeyTypes = []string{"ed25519", "bls12_381"}

	genDoc := &cmttypes.GenesisDoc{
		ChainID:         *chainID,
		GenesisTime:     time.Now().UTC(),
		ConsensusParams: consParams,
		Validators:      genesisValidators,
	}

	// Write genesis to every node
	for i := 0; i < *numNodes; i++ {
		genFile := filepath.Join(*outDir, fmt.Sprintf("node%d", i), "config", "genesis.json")
		if err := genDoc.SaveAs(genFile); err != nil {
			fatalf("save genesis for node%d: %v", i, err)
		}
	}

	// ── Step 3: Write config.toml for each node ────────────────────────────────
	configTemplate := `# SupernovaCore testnet config for node%d
log_level = "info"

[p2p]
laddr = "tcp://0.0.0.0:%d"
persistent_peers = "%s"
addr_book_strict = false

[rpc]
laddr = "tcp://127.0.0.1:%d"

[mempool]
version = "v1"

[consensus]
timeout_propose = "3s"
timeout_propose_delta = "500ms"
timeout_prevote = "1s"
timeout_prevote_delta = "500ms"
timeout_precommit = "1s"
timeout_precommit_delta = "500ms"
timeout_commit = "1s"
`

	for i := 0; i < *numNodes; i++ {
		// Build persistent_peers = all other nodes
		var peers []string
		for j := 0; j < *numNodes; j++ {
			if j == i {
				continue
			}
			peers = append(peers, fmt.Sprintf("%s@127.0.0.1:%d", nodeInfos[j].nodeID, p2pPorts[j]))
		}
		persistentPeers := strings.Join(peers, ",")

		content := fmt.Sprintf(configTemplate, i, p2pPorts[i], persistentPeers, rpcPorts[i])
		configFile := filepath.Join(*outDir, fmt.Sprintf("node%d", i), "config", "config.toml")
		if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
			fatalf("write config.toml for node%d: %v", i, err)
		}
	}

	// ── Step 4: Write summary JSON ─────────────────────────────────────────────
	type NodeSummary struct {
		Index           int    `json:"index"`
		NodeID          string `json:"node_id"`
		ValidatorAddr   string `json:"validator_addr"`
		P2PPort         int    `json:"p2p_port"`
		RPCPort         int    `json:"rpc_port"`
		Libp2pTCPPort   int    `json:"libp2p_tcp_port"`
		Libp2pUDPPort   int    `json:"libp2p_udp_port"`
		PersistentPeers string `json:"persistent_peers"`
	}

	var summaries []NodeSummary
	for i := 0; i < *numNodes; i++ {
		var peers []string
		for j := 0; j < *numNodes; j++ {
			if j != i {
				peers = append(peers, fmt.Sprintf("%s@127.0.0.1:%d", nodeInfos[j].nodeID, p2pPorts[j]))
			}
		}
		summaries = append(summaries, NodeSummary{
			Index:           i,
			NodeID:          nodeInfos[i].nodeID,
			ValidatorAddr:   nodeInfos[i].addr,
			P2PPort:         p2pPorts[i],
			RPCPort:         rpcPorts[i],
			Libp2pTCPPort:   p2pPorts[i] + 1000,
			Libp2pUDPPort:   p2pPorts[i] + 2000,
			PersistentPeers: strings.Join(peers, ","),
		})
	}

	summaryData, _ := json.MarshalIndent(summaries, "", "  ")

	nodesFile := filepath.Join(*outDir, "nodes.json")
	if err := os.WriteFile(nodesFile, summaryData, 0644); err != nil {
		fatalf("write nodes.json: %v", err)
	}

	fmt.Printf("\n%s\n", string(summaryData))
	fmt.Printf("\n✅ Testnet config generated in %s\n", *outDir)
	fmt.Printf("   ChainID:            %s\n", *chainID)
	fmt.Printf("   Nodes:              %d\n", *numNodes)
	fmt.Printf("   Validator key type: bls12_381\n")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "❌ "+format+"\n", args...)
	os.Exit(1)
}
