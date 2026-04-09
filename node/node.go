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
	"os"
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
	"github.com/spf13/viper"

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
	if err != nil {
		return nil, fmt.Errorf("opening main db (use a different -cmt-home per node): %w", err)
	}
	if mainDB == nil {
		return nil, fmt.Errorf("main db is nil")
	}

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
	slog.Info("Connecting to ABCI app (ensure Cosmos/simd is running on 26658 first)", "proxy_app", config.ProxyApp)
	connectCtx, connectCancel := context.WithTimeout(ctx, 20*time.Second)
	defer connectCancel()
	proxyApp, err := createAndStartProxyAppConnsWithContext(connectCtx, clientCreator, cmtproxy.NopMetrics())
	if err != nil {
		if connectCtx.Err() != nil {
			return nil, fmt.Errorf("ABCI connection timeout after 20s - is Cosmos app running? Start it first: bash scripts/start-cosmos-abci.sh (proxy_app=%s)", config.ProxyApp)
		}
		return nil, err
	}

	eventBus, err := createAndStartEventBus(logger)
	if err != nil {
		return nil, err
	}
	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, 15*time.Second)
	defer handshakeCancel()
	err = doHandshake(handshakeCtx, chain, genDoc, eventBus, proxyApp)
	if err != nil {
		if handshakeCtx.Err() != nil {
			return nil, fmt.Errorf("ABCI handshake timeout after 15s - app may not be responding (proxy_app=%s): %w", config.ProxyApp, err)
		}
		logger.Error("Handshake failed", "err", err)
		return nil, err
	}
	slog.Info("Handshake completed")
	txPool := txpool.New(chain, txpool.DefaultTxPoolOptions)

	// Bootstrap 节点：优先从环境变量 SUPERNOVA_BOOTSTRAP_ENRS 读取（逗号分隔的 ENR），否则从 config.P2P.Seeds 解析
	BootstrapNodes := getBootstrapNodes(config)

	geneBlock, err := chain.GetTrunkBlock(0)
	if err != nil {
		return nil, err
	}

	p2pSrv := newP2PService(ctx, config, BootstrapNodes, geneBlock)

	rpcServer := rpc.NewRPCServer(p2pSrv, chain, txPool)
	rpcServer.Start(ctx)

	communicator := rpc.NewCommunicator(ctx, chain, txPool, p2pSrv, rpcServer)
	p2pSrv.Host().Network().Notify(communicator)

	pacemaker := consensus.NewPacemaker(ctx, config.Version, chain, txPool, communicator, blsMaster, proxyApp)

	pubkey, err := privValidator.GetPubKey()

	apiAddr := rpcListenAddrToHostPort(config.RPC.ListenAddress)
	if apiAddr == "" {
		apiAddr = ":26657"
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
		mainDB:        mainDB,
	}
	go communicator.Start()

	return node, nil
}

func createAndStartProxyAppConns(clientCreator cmtproxy.ClientCreator, metrics *cmtproxy.Metrics) (proxy.AppConns, error) {
	proxyApp := proxy.NewAppConns(clientCreator, metrics)
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("error starting proxy app connections: %v", err)
	}
	return proxyApp, nil
}

// createAndStartProxyAppConnsWithContext runs proxy start with a timeout so we fail fast if ABCI is unreachable.
func createAndStartProxyAppConnsWithContext(ctx context.Context, clientCreator cmtproxy.ClientCreator, metrics *cmtproxy.Metrics) (proxy.AppConns, error) {
	type result struct {
		app proxy.AppConns
		err error
	}
	done := make(chan result, 1)
	go func() {
		app, err := createAndStartProxyAppConns(clientCreator, metrics)
		done <- result{app, err}
	}()
	select {
	case r := <-done:
		return r.app, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// getBootstrapNodes 返回用于 discv5 的 bootstrap 节点列表（ENR 或 multiaddr）。
// 优先使用环境变量 SUPERNOVA_BOOTSTRAP_ENRS（逗号分隔），否则使用 config.P2P.Seeds。
func getBootstrapNodes(config *cmtcfg.Config) []string {
	if s := os.Getenv("SUPERNOVA_BOOTSTRAP_ENRS"); s != "" {
		return splitAndTrim(s)
	}
	if config.P2P.Seeds != "" {
		return splitAndTrim(config.P2P.Seeds)
	}
	return nil
}

func splitAndTrim(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// rpcListenAddrToHostPort 从 config.RPC.ListenAddress（如 "tcp://127.0.0.1:26657"）提取 host:port 供 API 监听。
func rpcListenAddrToHostPort(laddr string) string {
	laddr = strings.TrimSpace(laddr)
	if laddr == "" {
		return ""
	}
	if idx := strings.Index(laddr, "://"); idx >= 0 {
		laddr = laddr[idx+3:]
	}
	return strings.TrimSpace(laddr)
}

// getP2PPorts 从配置读取 P2P 的 QUIC/TCP/UDP 端口；未配置时不再用偏移推算，使用默认值。
// 在 config.toml 的 [p2p] 中配置：quic_port、tcp_port、udp_port（建议每个节点显式指定，避免跨机端口不一致）。
func getP2PPorts(config *cmtcfg.Config) (quic, tcp, udp uint) {
	const defaultQUIC = 13000
	const defaultTCP = 13000
	const defaultUDP = 12000
	q := viper.GetInt("p2p.quic_port")
	t := viper.GetInt("p2p.tcp_port")
	u := viper.GetInt("p2p.udp_port")
	if q > 0 && t > 0 && u > 0 {
		return uint(q), uint(t), uint(u)
	}
	// 未配齐时用默认值，不再根据 laddr 做加减偏移
	if q > 0 {
		quic = uint(q)
	} else {
		quic = defaultQUIC
	}
	if t > 0 {
		tcp = uint(t)
	} else {
		tcp = defaultTCP
	}
	if u > 0 {
		udp = uint(u)
	} else {
		udp = defaultUDP
	}
	return quic, tcp, udp
}

func newP2PService(ctx context.Context, config *cmtcfg.Config, bootstrapNodes []string, geneBlock *block.Block) *p2p.Service {
	quicPort, tcpPort, udpPort := getP2PPorts(config)
	svc, err := p2p.NewService(ctx, &p2p.Config{
		NoDiscovery:          false,
		Discv5BootStrapAddrs: p2p.ParseBootStrapAddrs(bootstrapNodes),
		StaticPeers:          bootstrapNodes, // 与 seeds/bootstrap ENR 一致，用于主动拨号连接，解决两节点 0 peer
		DataDir:              config.RootDir,
		PrivateKey:           "",
		StaticPeerID:         true,
		QUICPort:             quicPort,
		TCPPort:              tcpPort,
		UDPPort:              udpPort,
		MaxPeers:             uint(config.P2P.MaxNumInboundPeers),
		QueueSize:            1000,
		// AllowListCIDR:        cliCtx.String(cmd.P2PAllowList.Name),
		// DenyListCIDR:         slice.SplitCommaSeparated(cliCtx.StringSlice(cmd.P2PDenyList.Name)),
		// EnableUPnP:           cliCtx.Bool(cmd.EnableUPnPFlag.Name),
		// StateNotifier: n,
		// DB:            n.mainDB,
	}, geneBlock.NanoTimestamp(), geneBlock.NextValidatorsHash())
	if err != nil {
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
	slog.Info("Starting handshake")
	if err := handshaker.Handshake(ctx, proxyApp); err != nil {
		slog.Error("Handshake failed", "err", err)
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

// CloseDB closes the chain database so state is flushed to disk.
// Call this after Start() returns (e.g. after context cancel on shutdown) to avoid
// "invalid height: 1; expected: N" on next run when process was killed without flush.
func (n *Node) CloseDB() {
	if n.mainDB != nil {
		if err := n.mainDB.Close(); err != nil {
			n.logger.Error("closing main DB", "err", err)
		}
		n.mainDB = nil
	}
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

	// 每分钟打印一次：当前区块高度、以及本分钟出块数（当前高度 - 上次打印时高度）
	blocksPerMinTicker := time.NewTicker(time.Minute)
	defer blocksPerMinTicker.Stop()
	lastHeightForMinute := uint32(0)
	if best := n.chain.BestBlock(); best != nil {
		lastHeightForMinute = best.Number()
	}

	futureBlocks := cache.NewRandCache(32)

	for {
		select {
		case <-ctx.Done():
			n.logger.Info("house keeping quite due to ctx done")
			return
		case <-blocksPerMinTicker.C:
			if best := n.chain.BestBlock(); best != nil {
				cur := best.Number()
				delta := uint32(0)
				if cur > lastHeightForMinute {
					delta = cur - lastHeightForMinute
				}
				if delta > 0 {
					perBlockMs := 60 * 1000 / uint32(delta)
					n.logger.Error("blocks in last minute", "height", cur, "delta", delta, "blocks_per_min", delta, "per_block_time_ms", perBlockMs)
				} else {
					// delta=0: no new blocks; log hint for debugging (not in committee => rely on NotifyBlock; no peers => no sync source)
					inCommittee := n.pacemaker != nil && n.pacemaker.InCommittee()
					peerCount := 0
					if n.communicator != nil {
						peerCount = n.communicator.PeerCount()
					}
					n.logger.Error("blocks in last minute", "height", cur, "delta", delta, "blocks_per_min", delta, "in_committee", inCommittee, "peer_count", peerCount)
				}
				lastHeightForMinute = cur
			}
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
