// Copyright (c) 2020 The Meter.io developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beevik/ntp"
	cmtcfg "github.com/cometbft/cometbft/v2/config"
	"github.com/cometbft/cometbft/v2/libs/log"
	"github.com/cometbft/cometbft/v2/privval"
	"github.com/cometbft/cometbft/v2/proxy"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/meterio/supernova/libs/p2p"
	"github.com/meterio/supernova/libs/rpc"

	db "github.com/cometbft/cometbft-db"
	cmtnode "github.com/cometbft/cometbft/v2/node"
	cmtproxy "github.com/cometbft/cometbft/v2/proxy"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/meterio/supernova/api"
	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/chain"
	"github.com/meterio/supernova/consensus"
	"github.com/meterio/supernova/libs/cache"
	"github.com/meterio/supernova/libs/co"
	"github.com/meterio/supernova/txpool"
	"github.com/meterio/supernova/types"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
)

var (
	GlobNodeInst           *Node
	errCantExtendBestBlock = errors.New("can't extend best block")
	genesisDocHashKey      = []byte("genesisDocHash")
)

var (
	errFutureBlock   = errors.New("block in the future")
	errParentMissing = errors.New("parent block is missing")
	errKnownBlock    = errors.New("block already in the chain")
)

func LoadGenesisDoc(
	mainDB db.DB,
	genesisDocProvider cmtnode.GenesisDocProvider,
) (*cmttypes.GenesisDoc, error) { // originally, LoadStateFromDBOrGenesisDocProvider
	// Get genesis doc hash
	genDocHash, err := mainDB.Get(genesisDocHashKey)
	if err != nil {
		return nil, fmt.Errorf("error retrieving genesis doc hash: %w", err)
	}
	csGenDoc, err := genesisDocProvider()
	if err != nil {
		return nil, err
	}

	if err = csGenDoc.GenesisDoc.ValidateAndComplete(); err != nil {
		return nil, fmt.Errorf("error in genesis doc: %w", err)
	}

	if len(genDocHash) == 0 {
		// Save the genDoc hash in the store if it doesn't already exist for future verification
		if err = mainDB.SetSync(genesisDocHashKey, csGenDoc.Sha256Checksum); err != nil {
			return nil, fmt.Errorf("failed to save genesis doc hash to db: %w", err)
		}
	} else {
		if !bytes.Equal(genDocHash, csGenDoc.Sha256Checksum) {
			return nil, errors.New("genesis doc hash in db does not match loaded genesis doc")
		}
	}

	return csGenDoc.GenesisDoc, nil
}

type Node struct {
	goes          co.Goes
	config        *cmtcfg.Config
	ctx           context.Context
	genesisDoc    *cmttypes.GenesisDoc   // initial validator set
	privValidator cmttypes.PrivValidator // local node's validator key
	communicator  *rpc.Communicator

	apiServer *api.APIServer

	// network
	nodeKey   *types.NodeKey // our node privkey
	pacemaker *consensus.Pacemaker

	chain       *chain.Chain
	txPool      *txpool.TxPool
	txStashPath string
	rpcServer   *rpc.RPCServer
	logger      *slog.Logger

	mainDB db.DB
	p2pSrv p2p.P2P

	proxyApp cmtproxy.AppConns
}

// Chain returns the chain for block/chain data access (e.g. RPC Local client).
func (n *Node) Chain() *chain.Chain { return n.chain }

// TxPool returns the transaction pool (e.g. for BroadcastTx, Mempool queries).
func (n *Node) TxPool() *txpool.TxPool { return n.txPool }

// ProxyApp returns the ABCI proxy connections (Query, Mempool, Consensus).
func (n *Node) ProxyApp() cmtproxy.AppConns { return n.proxyApp }

// Communicator returns the communicator for block events (e.g. Subscribe).
func (n *Node) Communicator() *rpc.Communicator { return n.communicator }

// GenesisDoc returns the genesis document.
func (n *Node) GenesisDoc() *cmttypes.GenesisDoc { return n.genesisDoc }

// Pacemaker returns the consensus pacemaker.
func (n *Node) Pacemaker() *consensus.Pacemaker { return n.pacemaker }

// P2P returns the P2P service for peer information (e.g. NetInfo RPC).
func (n *Node) P2P() p2p.P2P { return n.p2pSrv }

func NewNode(
	ctx context.Context,
	config *cmtcfg.Config,
	privValidator *privval.FilePV,
	nodeKey *types.NodeKey,
	clientCreator cmtproxy.ClientCreator,
	genesisDocProvider cmtnode.GenesisDocProvider,
	dbProvider cmtcfg.DBProvider,
	metricProvider cmtnode.MetricsProvider,
	logger log.Logger,
) (*Node, error) {
	InitLogger(config)

	mainDB, err := dbProvider(&cmtcfg.DBContext{ID: "maindb", Config: config})

	genDoc, err := LoadGenesisDoc(mainDB, genesisDocProvider)
	if err != nil {
		return nil, err
	}

	chain := NewChain(mainDB)

	// if flattern index start is not set, or pruning is not complete
	// start the pruning routine right now

	privValidator.Key.PubKey.Bytes()
	blsMaster := types.NewBlsMasterWithCometKeys(privValidator.Key.PrivKey, privValidator.Key.PubKey)

	slog.Info("Supernova start ...", "version", config.BaseConfig.Version)

	// Create the proxyApp and establish connections to the ABCI app (consensus, mempool, query).
	proxyApp, err := createAndStartProxyAppConns(clientCreator, cmtproxy.NopMetrics())
	if err != nil {
		return nil, err
	}

	eventBus, err := createAndStartEventBus(logger)
	if err != nil {
		return nil, err
	}
	err = doHandshake(ctx, chain, genDoc, eventBus, proxyApp)
	if err != nil {
		logger.Error("Handshake failed", "err", err)
		return nil, err
	}

	txPool := txpool.New(chain, txpool.DefaultTxPoolOptions)

	bootstrapNodes := loadBootstrapNodes(config.RootDir)

	geneBlock, err := chain.GetTrunkBlock(0)
	if err != nil {
		return nil, err
	}

	p2pSrv := newP2PService(ctx, config, bootstrapNodes, geneBlock)

	rpcServer := rpc.NewRPCServer(p2pSrv, chain, txPool)
	rpcServer.Start(ctx)

	communicator := rpc.NewCommunicator(ctx, chain, txPool, p2pSrv, rpcServer)
	p2pSrv.Host().Network().Notify(communicator)

	pacemaker := consensus.NewPacemaker(ctx, config.Version, chain, txPool, communicator, blsMaster, proxyApp)

	pubkey, err := privValidator.GetPubKey()

	// Derive API address from CometBFT RPC config, default to :26657
	apiAddr := ":26657"
	if config.RPC.ListenAddress != "" {
		// config.RPC.ListenAddress is e.g. "tcp://127.0.0.1:26657" or "tcp://127.0.0.1:26757"
		if idx := strings.LastIndex(config.RPC.ListenAddress, ":"); idx >= 0 {
			apiAddr = config.RPC.ListenAddress[idx:] // e.g. ":26757"
		}
	}
	chainId, err := strconv.ParseUint(genDoc.ChainID, 10, 64)
	apiServer := api.NewAPIServer(proxyApp.Query(), apiAddr, chainId, config.BaseConfig.Version, chain, txPool, pacemaker, pubkey.Bytes(), p2pSrv)

	bestBlock := chain.BestBlock()

	fmt.Printf(`Starting %v
    Network         [ %v ]    
    Best block      [ %v #%v @%v ]
    Forks           [ %v ]
    PubKey          [ %v ]
    API portal      [ %v ]
`,
		types.MakeName("Supernova", config.BaseConfig.Version),
		geneBlock.ID(),
		bestBlock.ID(), bestBlock.Number(), time.Unix(0, int64(bestBlock.NanoTimestamp())),
		types.GetForkConfig(geneBlock.ID()),
		hex.EncodeToString(pubkey.Bytes()),
		apiAddr)

	node := &Node{
		ctx:           ctx,
		pacemaker:     pacemaker,
		config:        config,
		genesisDoc:    genDoc,
		rpcServer:     rpcServer,
		communicator:  communicator,
		privValidator: privValidator,
		nodeKey:       nodeKey,
		apiServer:     apiServer,
		chain:         chain,
		txPool:        txPool,
		p2pSrv:        p2pSrv,
		logger:        slog.With("pkg", "node"),
		proxyApp:      proxyApp,
	}
	go communicator.Start()

	return node, nil
}

// loadBootstrapNodes reads P2P bootstrap nodes from config/supernova.toml.
// Returns nil or empty slice if file is missing or bootstrap_nodes is empty (single-node mode).
func loadBootstrapNodes(rootDir string) []string {
	cfgPath := filepath.Join(rootDir, "config", "supernova.toml")
	v := viper.New()
	v.SetConfigFile(cfgPath)
	v.SetConfigType("toml")
	if err := v.ReadInConfig(); err != nil {
		slog.Debug("Failed to read supernova.toml", "path", cfgPath, "err", err)
		return nil
	}
	raw := v.Get("p2p.bootstrap_nodes")
	if raw == nil {
		slog.Debug("p2p.bootstrap_nodes not found in config")
		return nil
	}
	slog.Debug("Found p2p.bootstrap_nodes in config", "type", fmt.Sprintf("%T", raw))
	// viper may return []interface{} for toml arrays
	switch arr := raw.(type) {
	case []string:
		slog.Info("Loaded bootstrap nodes (string array)", "count", len(arr), "nodes", arr)
		return arr
	case []interface{}:
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		slog.Info("Loaded bootstrap nodes (interface array)", "count", len(out), "nodes", out)
		return out
	default:
		slog.Warn("Unexpected type for p2p.bootstrap_nodes", "type", fmt.Sprintf("%T", arr))
		return nil
	}
}

func createAndStartProxyAppConns(clientCreator cmtproxy.ClientCreator, metrics *cmtproxy.Metrics) (proxy.AppConns, error) {
	proxyApp := proxy.NewAppConns(clientCreator, metrics)
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("error starting proxy app connections: %v", err)
	}
	return proxyApp, nil
}

func newP2PService(ctx context.Context, config *cmtcfg.Config, bootstrapNodes []string, geneBlock *block.Block) *p2p.Service {
	// Derive libp2p ports from the CometBFT P2P listen address to avoid conflicts
	// when multiple nodes run on the same host.
	// CometBFT P2P port: e.g. 26656, 26756, 26856, 26956
	// libp2p TCP/QUIC port = cmt_p2p_port + 1000  → 27656, 27756, ...
	// libp2p UDP/discv5 port = cmt_p2p_port + 2000 → 28656, 28756, ...
	p2pListenAddr := config.P2P.ListenAddress // e.g. "tcp://0.0.0.0:26656"
	tcpPort := 13000
	udpPort := 12000
	if p2pListenAddr != "" {
		// Extract port from the address (last ':' segment)
		if idx := strings.LastIndex(p2pListenAddr, ":"); idx >= 0 {
			if portStr := p2pListenAddr[idx+1:]; portStr != "" {
				if p, err := strconv.Atoi(portStr); err == nil {
					tcpPort = p + 1000
					udpPort = p + 2000
				}
			}
		}
	}
	slog.Info("Starting libp2p P2P service", "tcpPort", tcpPort, "udpPort", udpPort,
		"bootstrapCount", len(bootstrapNodes))

	// Parse bootstrap nodes for both discv5 and libp2p static peers
	discv5Addrs := p2p.ParseBootStrapAddrs(bootstrapNodes)
	staticPeers := p2p.ParseMultiAddrs(bootstrapNodes)

	slog.Info("Parsed bootstrap nodes",
		"discv5Count", len(discv5Addrs),
		"staticPeerCount", len(staticPeers),
		"discv5Addrs", discv5Addrs,
		"staticPeers", staticPeers)

	svc, err := p2p.NewService(ctx, &p2p.Config{
		NoDiscovery:          false,
		StaticPeers:          staticPeers,
		Discv5BootStrapAddrs: discv5Addrs,
		// RelayNodeAddr:        cliCtx.String(cmd.RelayNode.Name),
		DataDir: config.RootDir,
		// LocalIP:              cliCtx.String(cmd.P2PIP.Name),
		// HostAddress: config.P2P.ExternalAddress,
		// HostDNS:      cliCtx.String(cmd.P2PHostDNS.Name),
		PrivateKey:   "",
		StaticPeerID: true,
		// MetaDataDir:          cliCtx.String(cmd.P2PMetadata.Name),
		QUICPort:  uint(tcpPort),
		TCPPort:   uint(tcpPort),
		UDPPort:   uint(udpPort),
		MaxPeers:  uint(config.P2P.MaxNumInboundPeers),
		QueueSize: 1000,
		// AllowListCIDR:        cliCtx.String(cmd.P2PAllowList.Name),
		// DenyListCIDR:         slice.SplitCommaSeparated(cliCtx.StringSlice(cmd.P2PDenyList.Name)),
		// EnableUPnP:           cliCtx.Bool(cmd.EnableUPnPFlag.Name),
		// StateNotifier: n,
		// DB:            n.mainDB,
	}, geneBlock.NanoTimestamp(), geneBlock.NextValidatorsHash())
	if err != nil {
		slog.Error("p2p.NewService failed", "err", err)
		return nil
	}
	pubsub.WithSubscriptionFilter(pubsub.NewAllowlistSubscriptionFilter(p2p.ConsensusTopic))
	p := svc.PubSub()

	// for _, s := range p.GetTopics() {
	// 	fmt.Println("Valid topic: ", s)
	// }
	p.RegisterTopicValidator(p2p.ConsensusTopic, customTopicValidator)

	svc.Start()
	return svc
}

func customTopicValidator(ctx context.Context, peerID peer.ID, msg *pubsub.Message) (pubsub.ValidationResult, error) {
	// Perform custom validation
	// Example: Check message size, content, etc.
	fmt.Println("Validating consensus topic message", msg.ID)
	if len(msg.Data) > 0 { // Simple validation for non-empty messages
		return pubsub.ValidationAccept, nil
	}
	return pubsub.ValidationReject, errors.New("rejected")
}

func doHandshake(
	ctx context.Context,
	c *chain.Chain,
	genDoc *cmttypes.GenesisDoc,
	eventBus cmttypes.BlockEventPublisher,
	proxyApp proxy.AppConns,
) error {
	handshaker := consensus.NewHandshaker(c, genDoc)
	handshaker.SetEventBus(eventBus)
	if err := handshaker.Handshake(ctx, proxyApp); err != nil {
		return fmt.Errorf("error during handshake: %v", err)
	}
	return nil
}

func createAndStartEventBus(logger log.Logger) (*cmttypes.EventBus, error) {
	eventBus := cmttypes.NewEventBus()
	eventBus.SetLogger(logger.With("module", "events"))
	if err := eventBus.Start(); err != nil {
		return nil, err
	}
	return eventBus, nil
}

func (n *Node) Start() error {
	n.logger.Info("Node Start")
	// n.rpc.Start(n.ctx)
	n.communicator.Sync(n.handleBlockStream)

	n.goes.Go(func() { n.apiServer.Start(n.ctx) })
	n.goes.Go(func() { n.houseKeeping(n.ctx) })
	// n.goes.Go(func() { n.txStashLoop(n.ctx) })
	n.goes.Go(func() {
		n.pacemaker.Start()
	})

	n.goes.Wait()
	return nil
}

func (n *Node) Stop() error {
	n.rpcServer.Stop()
	return nil
}

func (n *Node) handleBlockStream(ctx context.Context, stream <-chan *block.EscortedBlock) (err error) {
	n.logger.Debug("start to process block stream")
	defer n.logger.Debug("process block stream done", "err", err)
	var stats blockStats
	startTime := mclock.Now()

	report := func(block *block.Block, pending int) {
		n.logger.Info(fmt.Sprintf("imported blocks (%v) ", stats.processed), stats.LogContext(block.Header(), pending)...)
		stats = blockStats{}
		startTime = mclock.Now()
	}

	var blk *block.EscortedBlock
	for blk = range stream {
		n.logger.Debug("process block", "block", blk.Block.ID().ToBlockShortID())
		if err := n.processBlock(blk.Block, blk.EscortQC, &stats); err != nil {
			if err == errCantExtendBestBlock {
				best := n.chain.BestBlock()
				n.logger.Warn("process block failed", "num", blk.Block.Number(), "id", blk.Block.ID(), "best", best.Number(), "err", err.Error())
			} else {
				n.logger.Error("process block failed", "num", blk.Block.Number(), "id", blk.Block.ID(), "err", err.Error())
			}
			return err
		} else {
			// this processBlock happens after consensus SyncDone, need to broadcast
			// if n.comm.Synced {
			// FIXME: skip broadcast blocks only if synced
			// n.comm.BroadcastBlock(blk)
			// }
		}

		if stats.processed > 0 &&
			mclock.Now()-startTime > mclock.AbsTime(time.Second*2) {
			report(blk.Block, len(stream))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if blk != nil && stats.processed > 0 {
		report(blk.Block, len(stream))
	}
	return nil
}

func (n *Node) houseKeeping(ctx context.Context) {
	n.logger.Info("enter house keeping")
	defer n.logger.Info("leave house keeping")

	var scope event.SubscriptionScope
	defer scope.Close()

	newBlockCh := make(chan *rpc.NewBlockEvent)
	scope.Track(n.communicator.SubscribeBlock(newBlockCh))

	futureTicker := time.NewTicker(time.Duration(types.BlockIntervalNano) * time.Nanosecond)
	defer futureTicker.Stop()

	connectivityTicker := time.NewTicker(time.Second)
	defer connectivityTicker.Stop()

	futureBlocks := cache.NewRandCache(32)

	for {
		select {
		case <-ctx.Done():
			n.logger.Info("house keeping quite due to ctx done")
			return
		case evt := <-newBlockCh:
			n.logger.Debug("found new block", "num", evt.NewBlock.Block.Number(), "id", evt.NewBlock.Block.ID())
			var stats blockStats

			if err := n.processBlock(evt.NewBlock.Block, evt.NewBlock.EscortQC, &stats); err != nil {
				if consensus.IsFutureBlock(err) ||
					(consensus.IsParentMissing(err) && futureBlocks.Contains(evt.NewBlock.Block.Header().ParentID)) {
					n.logger.Debug("future block added", "id", evt.NewBlock.Block.ID())
					futureBlocks.Set(evt.NewBlock.Block.ID(), evt.NewBlock)
				}
			} else {
				// TODO: maybe I should not broadcast future blocks？
				// n.comm.BroadcastBlock(newBlock.EscortedBlock)
			}
		case <-futureTicker.C:
			// process future blocks
			var blocks []*block.EscortedBlock
			futureBlocks.ForEach(func(ent *cache.Entry) bool {
				blocks = append(blocks, ent.Value.(*block.EscortedBlock))
				return true
			})
			sort.Slice(blocks, func(i, j int) bool {
				return blocks[i].Block.Number() < blocks[j].Block.Number()
			})
			var stats blockStats
			for i, block := range blocks {
				if err := n.processBlock(block.Block, block.EscortQC, &stats); err == nil || consensus.IsKnownBlock(err) {
					n.logger.Debug("future block consumed", "id", block.Block.ID())
					futureBlocks.Remove(block.Block.ID())
					// n.comm.BroadcastBlock(block)
				}

				if stats.processed > 0 && i == len(blocks)-1 {
					// n.logger.Info(fmt.Sprintf("imported blocks (%v)", stats.processed), stats.LogContext(block.Header())...)
				}
			}
		case <-connectivityTicker.C:
			// if n.comm.PeerCount() == 0 {
			// 	noPeerTimes++
			// 	if noPeerTimes > 30 {
			// 		noPeerTimes = 0
			// 		go checkClockOffset()
			// 	}
			// } else {
			// 	noPeerTimes = 0
			// }
		}
	}
}

// func (n *Node) txStashLoop(ctx context.Context) {
// 	n.logger.Debug("enter tx stash loop")
// 	defer n.logger.Debug("leave tx stash loop")

// 	db, err := lvldb.New(n.txStashPath, lvldb.Options{})
// 	if err != nil {
// 		n.logger.Error("create tx stash", "err", err)
// 		return
// 	}
// 	defer db.Close()

// 	stash := newTxStash(db, 1000)

// 	{
// 		txs := stash.LoadAll()
// 		bestBlock := n.chain.BestBlock()
// 		n.txPool.Fill(txs, func(txID []byte) bool {
// 			if _, err := n.chain.GetTransactionMeta(txID, bestBlock.ID()); err != nil {
// 				return false
// 			} else {
// 				return true
// 			}
// 		})
// 		n.logger.Debug("loaded txs from stash", "count", len(txs))
// 	}

// 	var scope event.SubscriptionScope
// 	defer scope.Close()

// 	txCh := make(chan *txpool.TxEvent)
// 	scope.Track(n.txPool.SubscribeTxEvent(txCh))
// 	for {
// 		select {
// 		case <-ctx.Done():
// 			return
// 		case txEv := <-txCh:
// 			// skip executables
// 			if txEv.Executable != nil && *txEv.Executable {
// 				continue
// 			}
// 			// only stash non-executable txs
// 			if err := stash.Save(txEv.Tx); err != nil {
// 				n.logger.Warn("stash tx", "id", txEv.Tx.Hash(), "err", err)
// 			} else {
// 				n.logger.Debug("stashed tx", "id", txEv.Tx.Hash())
// 			}
// 		}
// 	}
// }

type syncError string

func (err syncError) Error() string {
	return string(err)
}

func (n *Node) processBlock(blk *block.Block, escortQC *block.QuorumCert, stats *blockStats) error {
	nowNano := uint64(time.Now().UnixNano())

	best := n.chain.BestBlock()
	if !bytes.Equal(best.ID().Bytes(), blk.ParentID().Bytes()) {
		return errCantExtendBestBlock
	}
	if blk.NanoTimestamp()+types.BlockIntervalNano > nowNano {
		QCValid := n.pacemaker.ValidateQC(blk, escortQC)
		if !QCValid {
			return errors.New(fmt.Sprintf("invalid %s on Block %s", escortQC.String(), blk.ID().ToBlockShortID()))
		}
	}
	start := time.Now()
	err := n.ValidateSyncedBlock(blk, nowNano)
	if time.Since(start) > time.Millisecond*500 {
		n.logger.Debug("slow processed block", "blk", blk.Number(), "elapsed", types.PrettyDuration(time.Since(start)))
	}

	if err != nil {
		switch {
		case consensus.IsKnownBlock(err):
			return nil
		case consensus.IsFutureBlock(err) || consensus.IsParentMissing(err):
			return nil
		case consensus.IsCritical(err):
			msg := fmt.Sprintf(`failed to process block due to consensus failure \n%v\n`, blk.Header())
			n.logger.Error(msg, "err", err)
		default:
			n.logger.Error("failed to process block", "err", err)
		}
		return err
	}
	start = time.Now()
	err = n.pacemaker.CommitBlock(blk, escortQC)
	if err != nil {
		if !n.chain.IsBlockExist(err) {
			n.logger.Error("failed to commit block", "err", err)
		}
		return err
	}

	stats.UpdateProcessed(1, len(blk.Txs))
	// n.processFork(fork)

	return nil
}

// func (n *Node) processFork(fork *chain.Fork) {
// 	if len(fork.Branch) >= 2 {
// 		trunkLen := len(fork.Trunk)
// 		branchLen := len(fork.Branch)
// 		n.logger.Warn(fmt.Sprintf(
// 			`⑂⑂⑂⑂⑂⑂⑂⑂ FORK HAPPENED ⑂⑂⑂⑂⑂⑂⑂⑂
// ancestor: %v
// trunk:    %v  %v
// branch:   %v  %v`, fork.Ancestor,
// 			trunkLen, fork.Trunk[trunkLen-1],
// 			branchLen, fork.Branch[branchLen-1]))
// 	}
// 	for _, header := range fork.Branch {
// 		body, err := n.chain.GetBlockBody(header.ID())
// 		if err != nil {
// 			n.logger.Warn("failed to get block body", "err", err, "blockid", header.ID())
// 			continue
// 		}
// 		for _, tx := range body.Txs {
// 			if err := n.txPool.Add(tx); err != nil {
// 				n.logger.Debug("failed to add tx to tx pool", "err", err, "id", tx.Hash())
// 			}
// 		}
// 	}
// }

func checkClockOffset() {
	resp, err := ntp.Query("ap.pool.ntp.org")
	if err != nil {
		slog.Debug("failed to access NTP", "err", err)
		return
	}
	if resp.ClockOffset > time.Duration(types.BlockIntervalNano)*time.Nanosecond/2 {
		slog.Warn("clock offset detected", "offset", types.PrettyDuration(resp.ClockOffset))
	}
}

func (n *Node) IsRunning() bool {
	// FIXME: set correct value
	return true
}

// Process process a block.
func (n *Node) ValidateSyncedBlock(blk *block.Block, nowNanoTimestamp uint64) error {
	header := blk.Header()

	if _, err := n.chain.GetBlockHeader(header.ID()); err != nil {
		if !n.chain.IsNotFound(err) {
			return err
		}
	} else {
		// we may already have this blockID. If it is after the best, still accept it
		if header.Number() <= n.chain.BestBlock().Number() {
			return errKnownBlock
		} else {
			n.logger.Debug("continue to process blk ...", "height", header.Number())
		}
	}

	parent, err := n.chain.GetBlock(header.ParentID)
	if err != nil {
		if !n.chain.IsNotFound(err) {
			return err
		}
		return errParentMissing
	}

	return n.validateBlock(blk, parent, nowNanoTimestamp, true)
}

func (n *Node) validateBlock(
	block *block.Block,
	parent *block.Block,
	nowNanoTimestamp uint64,
	forceValidate bool,
) error {
	header := block.Header()

	start := time.Now()
	if parent == nil {
		return syncError("parent is nil")
	}

	if parent.ID() != block.QC.BlockID {
		return syncError(fmt.Sprintf("parent.ID %v and QC.BlockID %v mismatch", parent.ID(), block.QC.BlockID))
	}

	if header.NanoTimestamp <= parent.NanoTimestamp() {
		return syncError(fmt.Sprintf("block nano timestamp behind parents: parent %v, current %v", parent.NanoTimestamp, header.NanoTimestamp))
	}

	if header.NanoTimestamp > nowNanoTimestamp+types.BlockIntervalNano {
		return errFutureBlock
	}

	if header.LastKBlock < parent.LastKBlock() {
		return syncError(fmt.Sprintf("block LastKBlock invalid: parent %v, current %v", parent.LastKBlock, header.LastKBlock))
	}

	proposedTxs := block.Transactions()
	if !bytes.Equal(header.TxsRoot, proposedTxs.RootHash()) {
		return syncError(fmt.Sprintf("block txs root mismatch: want %v, have %v", header.TxsRoot, proposedTxs.RootHash()))
	}

	n.logger.Debug("validated block", "id", block.CompactString(), "elapsed", types.PrettyDuration(time.Since(start)))
	return nil
}
