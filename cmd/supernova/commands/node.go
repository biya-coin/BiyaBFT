package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	cmtcfg "github.com/cometbft/cometbft/v2/config"
	cmtflags "github.com/cometbft/cometbft/v2/libs/cli/flags"
	cmtlog "github.com/cometbft/cometbft/v2/libs/log"
	cmtnode "github.com/cometbft/cometbft/v2/node"
	"github.com/cometbft/cometbft/v2/privval"
	"github.com/cometbft/cometbft/v2/proxy"
	cmn "github.com/meterio/supernova/libs/common"
	"github.com/meterio/supernova/genesis"
	"github.com/meterio/supernova/node"
	"github.com/meterio/supernova/types"
	"github.com/spf13/cobra"
)

var (
	genesisHash []byte
)

// AddNodeFlags exposes some common configuration options on the command-line
// These are exposed for convenience of commands embedding a CometBFT node
func AddNodeFlags(cmd *cobra.Command) {
	// bind flags
	cmd.Flags().String("moniker", config.Moniker, "node name")

	// priv val flags
	cmd.Flags().String(
		"priv_validator_laddr",
		config.PrivValidatorListenAddr,
		"socket address to listen on for connections from external priv_validator process")

	// node flags
	cmd.Flags().BytesHexVar(
		&genesisHash,
		"genesis_hash",
		[]byte{},
		"optional SHA-256 hash of the genesis file")
	cmd.Flags().Int64("consensus.double_sign_check_height", config.Consensus.DoubleSignCheckHeight,
		"how many blocks to look back to check existence of the node's "+
			"consensus votes before joining consensus")

	// abci flags
	cmd.Flags().String(
		"proxy_app",
		config.ProxyApp,
		"ABCI app address for remote connection (e.g. tcp://127.0.0.1:26658 for biyachain-core), or built-in: 'kvstore', 'persistent_kvstore', 'noop'")
	cmd.Flags().String("abci", config.ABCI, "ABCI transport: socket or grpc (use grpc for dual-process with biyachain-core)")

	// rpc flags
	cmd.Flags().String("rpc.laddr", config.RPC.ListenAddress, "RPC listen address. Port required")
	// cmd.Flags().String(
	// 	"rpc.grpc_laddr",
	// 	config.RPC.GRPCListenAddress,
	// 	"GRPC listen address (BroadcastTx only). Port required")
	cmd.Flags().Bool("rpc.unsafe", config.RPC.Unsafe, "enabled unsafe rpc methods")
	cmd.Flags().String("rpc.pprof_laddr", config.RPC.PprofListenAddress, "pprof listen address (https://golang.org/pkg/net/http/pprof)")

	// p2p flags
	cmd.Flags().String(
		"p2p.laddr",
		config.P2P.ListenAddress,
		"node listen address. (0.0.0.0:0 means any interface, any port)")
	cmd.Flags().String("p2p.external-address", config.P2P.ExternalAddress, "ip:port address to advertise to peers for them to dial")
	cmd.Flags().String("p2p.seeds", config.P2P.Seeds, "comma-delimited ID@host:port seed nodes")
	cmd.Flags().String("p2p.persistent_peers", config.P2P.PersistentPeers, "comma-delimited ID@host:port persistent peers")
	cmd.Flags().String("p2p.unconditional_peer_ids",
		config.P2P.UnconditionalPeerIDs, "comma-delimited IDs of unconditional peers")
	cmd.Flags().Bool("p2p.pex", config.P2P.PexReactor, "enable/disable Peer-Exchange")
	cmd.Flags().Bool("p2p.seed_mode", config.P2P.SeedMode, "enable/disable seed mode")
	cmd.Flags().String("p2p.private_peer_ids", config.P2P.PrivatePeerIDs, "comma-delimited private peer IDs")
	cmd.Flags().Uint("p2p.quic_port", 0, "P2P QUIC listen port (0=use default 13000; set in config.toml [p2p] quic_port for each node)")
	cmd.Flags().Uint("p2p.tcp_port", 0, "P2P TCP listen port (0=use default 13000)")
	cmd.Flags().Uint("p2p.udp_port", 0, "P2P UDP/discv5 listen port (0=use default 12000)")

	// consensus flags
	cmd.Flags().Bool(
		"consensus.create_empty_blocks",
		config.Consensus.CreateEmptyBlocks,
		"set this to false to only produce blocks when there are txs or when the AppHash changes")
	cmd.Flags().String(
		"consensus.create_empty_blocks_interval",
		config.Consensus.CreateEmptyBlocksInterval.String(),
		"the possible interval between empty blocks")

	// db flags
	cmd.Flags().String(
		"db_backend",
		config.DBBackend,
		"database backend: goleveldb | cleveldb | boltdb | rocksdb | badgerdb")
	cmd.Flags().String(
		"db_dir",
		config.DBPath,
		"database directory")
}

func RunNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "start",
		Aliases: []string{"node", "run"},
		Short:   "Run the Supernova node",
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Info("Running node", "config", config)
			if err := checkGenesisHash(config); err != nil {
				return err
			}
			nodeKey, err := types.LoadOrGenNodeKey(config.NodeKeyFile())
			if err != nil {
				return fmt.Errorf("failed to load or gen node key %s: %w", config.NodeKeyFile(), err)
			}

			InitLogger(config)
			logger := cmtlog.NewLogger(os.Stdout)
			logger, err = cmtflags.ParseLogLevel(config.LogLevel, logger, cmtcfg.DefaultLogLevel)

			privValidator, _ := privval.LoadOrGenFilePV(config.PrivValidatorKeyFile(), config.PrivValidatorStateFile(), nil)
			// Use cancellable context so SIGTERM/CTRL-C can trigger graceful shutdown and DB flush.
			ctx, cancel := context.WithCancel(context.Background())
			// For gRPC, use host:port so grpc.NewClient doesn't block on "tcp://" target (cometbft dialer still gets host:port and dials tcp)
			proxyAddr := config.ProxyApp
			if config.ABCI == "grpc" && (strings.HasPrefix(proxyAddr, "tcp://") || strings.HasPrefix(proxyAddr, "unix://")) {
				proxyAddr = strings.SplitN(proxyAddr, "://", 2)[1]
			}
			node, err := node.NewNode(ctx, config,
				privValidator,
				nodeKey,
				proxy.DefaultClientCreator(proxyAddr, config.ABCI, config.DBDir()),
				genesis.AppGenesisDocProvider(config),
				cmtcfg.DefaultDBProvider,
				cmtnode.DefaultMetricsProvider(config.Instrumentation),
				logger,
			)
			if err != nil {
				return err
			}

			// On SIGTERM/CTRL-C: cancel context so goroutines exit, Start() returns, then we close DB and exit.
			cmn.TrapSignal(func() {
				if node.IsRunning() {
					slog.Info("Node stopping (cancelling context to flush chain DB)...")
					cancel()
				}
			})

			// Start() blocks until context is cancelled and all goroutines are done.
			_ = node.Start()
			// Brief delay so communicator and other non-goes goroutines see context cancel before we close DB.
			time.Sleep(500 * time.Millisecond)
			node.CloseDB()
			slog.Info("Node stopped, DB closed.")
			return nil
		},
	}
	AddNodeFlags(cmd)
	return cmd
}

func checkGenesisHash(config *cmtcfg.Config) error {
	slog.Info("Checking genesis hash", "genesisHash", genesisHash, "config.Genesis", config.Genesis)
	if len(genesisHash) == 0 || config.Genesis == "" {
		return nil
	}

	// Calculate SHA-256 hash of the genesis file.
	f, err := os.Open(config.GenesisFile())
	if err != nil {
		return fmt.Errorf("can't open genesis file: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("error when hashing genesis file: %w", err)
	}
	actualHash := h.Sum(nil)

	// Compare with the flag.
	if !bytes.Equal(genesisHash, actualHash) {
		return fmt.Errorf(
			"--genesis_hash=%X does not match %s hash: %X",
			genesisHash, config.GenesisFile(), actualHash)
	}

	return nil
}
