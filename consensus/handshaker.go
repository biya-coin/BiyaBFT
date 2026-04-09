package consensus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	abci "github.com/cometbft/cometbft/v2/abci/types"
	"github.com/cometbft/cometbft/v2/libs/log"
	"github.com/cometbft/cometbft/v2/proxy"
	sm "github.com/cometbft/cometbft/v2/state"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/chain"
	"github.com/meterio/supernova/genesis"
	"github.com/meterio/supernova/txpool"
)

type Handshaker struct {
	chain    *chain.Chain
	eventBus cmttypes.BlockEventPublisher
	genDoc   *cmttypes.GenesisDoc
	txPool   *txpool.TxPool // may be nil during handshake (node creates txPool after Handshake); replay only uses Executor.ApplyBlock which does not touch txPool
	logger   log.Logger

	nBlocks int // number of blocks applied to the state
}

func NewHandshaker(c *chain.Chain, genDoc *cmttypes.GenesisDoc,
) *Handshaker {
	return &Handshaker{
		chain:    c,
		eventBus: cmttypes.NopEventBus{},
		genDoc:   genDoc,
		logger:   log.NewNopLogger(),
		nBlocks:  0,
	}
}

func (h *Handshaker) SetLogger(l log.Logger) {
	h.logger = l
}

// SetEventBus - sets the event bus for publishing block related events.
// If not called, it defaults to types.NopEventBus.
func (h *Handshaker) SetEventBus(eventBus cmttypes.BlockEventPublisher) {
	h.eventBus = eventBus
}

// NBlocks returns the number of blocks applied to the state.
func (h *Handshaker) NBlocks() int {
	return h.nBlocks
}

// TODO: retry the handshake/replay if it fails ?
func (h *Handshaker) Handshake(ctx context.Context, proxyApp proxy.AppConns) error {
	// Handshake is done via ABCI Info on the query conn.
	res, err := proxyApp.Query().Info(ctx, proxy.InfoRequest)
	if err != nil {
		return fmt.Errorf("error calling Info: %v", err)
	}
	slog.Info("ABCI Handshake App Info", "res", res)
	blockHeight := res.LastBlockHeight
	if blockHeight < 0 {
		return fmt.Errorf("got a negative last block height (%d) from the app", blockHeight)
	}
	appHash := res.LastBlockAppHash

	h.logger.Info("ABCI Handshake App Info",
		"height", blockHeight,
		"hash", log.NewLazySprintf("%X", appHash),
		"software-version", res.Version,
		"protocol-version", res.AppVersion,
	)

	// best := h.chain.BestBlock()

	// // Only set the version if there is no existing state.
	// if best.Number() == 0 {
	// 	// h. = res.AppVersion
	// }

	// Replay blocks up to the latest in the blockstore.
	appHash, err = h.ReplayBlocks(ctx, appHash, blockHeight, proxyApp)
	if err != nil {
		var heightErr sm.ErrAppBlockHeightTooHigh
		if errors.As(err, &heightErr) {
			return fmt.Errorf("error on replay: %v (run scripts/reset-supernova-data.sh to resync app and consensus from genesis)", err)
		}
		return fmt.Errorf("error on replay: %v", err)
	}
	slog.Info("Completed ABCI Handshake - CometBFT and App are synced", "appHeight", blockHeight, "appHash", log.NewLazySprintf("%X", appHash))
	h.logger.Info("Completed ABCI Handshake - CometBFT and App are synced",
		"appHeight", blockHeight, "appHash", log.NewLazySprintf("%X", appHash))

	// TODO: (on restart) replay mempool

	return nil
}

// ReplayBlocks replays all blocks since appBlockHeight and ensures the result
// matches the current state.
// Returns the final AppHash or an error.
func (h *Handshaker) ReplayBlocks(
	ctx context.Context,
	appHash []byte,
	appBlockHeight int64,
	proxyApp proxy.AppConns,
) ([]byte, error) {

	// If appBlockHeight == 0 it means that we are at genesis and hence should send InitChain.
	if appBlockHeight == 0 {
		geneValidators := make([]*cmttypes.Validator, len(h.genDoc.Validators))
		for i, val := range h.genDoc.Validators {
			// Ensure that the public key type is supported.
			if _, ok := cmttypes.ABCIPubKeyTypesToNames[val.PubKey.Type()]; !ok {
				h.logger.Error("Unspported key type ", "pubkeyType", val.PubKey.Type(), "value", val.Name)
				return nil, fmt.Errorf("unsupported public key type %s (validator name: %s)", val.PubKey.Type(), val.Name)
			}
			geneValidators[i] = cmttypes.NewValidator(val.PubKey, val.Power)
		}
		geneVSet := cmttypes.NewValidatorSet(geneValidators)
		geneVUpdates := cmttypes.TM2PB.ValidatorUpdates(geneVSet)

		pbparams := h.genDoc.ConsensusParams.ToProto()
		// Send empty Validators so Cosmos BaseApp skips req.Validators == res.Validators check
		// (Cosmos returns validators from gen_txs with different proto encoding). We use res.Validators for genesis.
		req := &abci.InitChainRequest{
			Time:            h.genDoc.GenesisTime,
			ChainId:         h.genDoc.ChainID,
			InitialHeight:   h.genDoc.InitialHeight,
			ConsensusParams: &pbparams,
			Validators:      nil,
			AppStateBytes:   h.genDoc.AppState,
		}
		res, err := proxyApp.Consensus().InitChain(context.TODO(), req)
		if err != nil {
			h.logger.Error("InitChain failed", "err", err)
			return nil, err
		}
		slog.Info("InitChain Response", "res", res)
		appHash = res.AppHash

		// Do NOT Commit here. BaseApp expects the first FinalizeBlock to be height 1 (lastBlockHeight 0 + 1).
		// If we Commit, app.LastBlockHeight becomes 1 and the next FinalizeBlock(block 1) is rejected as "invalid height: 1; expected: 2".

		// When app (e.g. noop) returns empty Validators, use request validators so genesis is correct.
		validatorUpdates := res.Validators
		if len(validatorUpdates) == 0 {
			validatorUpdates = geneVUpdates
		}
		slog.Info("building genesis and initializing chain", "validator_updates", len(validatorUpdates))
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic during genesis/init", "panic", r)
				panic(r)
			}
		}()
		gene := genesis.NewGenesis(h.genDoc, validatorUpdates)
		slog.Info("genesis created, calling chain.Initialize")
		err = h.chain.Initialize(gene)
		if err != nil {
			h.logger.Error("chain initialize failed", "err", err)
			return nil, err
		}
		slog.Info("chain initialized successfully")

		// for i, v := range res.Validators {
		// fmt.Println(" ", i, ": ", v.PubKeyType, hex.EncodeToString(v.PubKeyBytes))
		// }
		err = h.chain.SaveInitChainResponse(res)
		if err != nil {
			h.logger.Error("Save InitChainResponse failed", "err", err)
		}
		slog.Info("saved InitChain response")

		initChainRes, err := h.chain.GetInitChainResponse()
		if err != nil {
			panic(err)
		}
		fmt.Println("Init Chain Response Validators: ", len(initChainRes.Validators))
	} else {
		initChainRes, err := h.chain.GetInitChainResponse()
		if err != nil {
			panic(err)
		}
		fmt.Println("InitChain Response Validators: ", len(initChainRes.Validators))
		validatorUpdates := initChainRes.Validators
		// Persisted InitChainResponse may have Validators with empty PubKeyBytes (e.g. proto only stored deprecated pub_key).
		// Use genesis doc validators so we have valid PubKey bytes for NewGenesis.
		hasEmptyPubKeyBytes := false
		for _, v := range initChainRes.Validators {
			if len(v.PubKeyBytes) == 0 {
				hasEmptyPubKeyBytes = true
				break
			}
		}
		if hasEmptyPubKeyBytes && len(h.genDoc.Validators) > 0 {
			geneValidators := make([]*cmttypes.Validator, len(h.genDoc.Validators))
			for i, val := range h.genDoc.Validators {
				if _, ok := cmttypes.ABCIPubKeyTypesToNames[val.PubKey.Type()]; !ok {
					h.logger.Error("Unsupported key type (genDoc fallback)", "pubkeyType", val.PubKey.Type(), "value", val.Name)
					return nil, fmt.Errorf("unsupported public key type %s (validator name: %s)", val.PubKey.Type(), val.Name)
				}
				geneValidators[i] = cmttypes.NewValidator(val.PubKey, val.Power)
			}
			geneVSet := cmttypes.NewValidatorSet(geneValidators)
			validatorUpdates = cmttypes.TM2PB.ValidatorUpdates(geneVSet)
			slog.Info("Using genesis doc validators (saved InitChainResponse had empty PubKeyBytes)", "count", len(validatorUpdates))
		}
		gene := genesis.NewGenesis(h.genDoc, validatorUpdates)

		err = h.chain.Initialize(gene)
		if err != nil {
			return nil, err
		}

	}

	best := h.chain.BestBlock()

	storeBlockHeight := uint32(0)
	if best != nil {
		storeBlockHeight = best.Number()
	}

	// First handle edge cases and constraints on the storeBlockHeight and storeBlockBase.
	switch {
	case storeBlockHeight == 0:
		slog.Info("handshake done (store at genesis), returning")
		return appHash, nil

	case int64(storeBlockHeight) < appBlockHeight:
		// the app should never be ahead of the store (but this is under app's control)
		return appHash, sm.ErrAppBlockHeightTooHigh{CoreHeight: int64(storeBlockHeight), AppHeight: appBlockHeight}

	}

	// Now either store is equal to state, or one ahead.
	// For each, consider all cases of where the app could be, given app <= store

	// CometBFT ran Commit and saved the state.
	// Either the app is asking for replay, or we're all synced up.
	if appBlockHeight < int64(storeBlockHeight) {
		// the app is behind, so replay blocks, but no need to go through WAL (state is already synced to store)
		return h.replayBlocks(ctx, proxyApp, appBlockHeight, int64(storeBlockHeight), false)
	} else if appBlockHeight == int64(storeBlockHeight) {
		// We're good!
		assertAppHashEqualsOneFromBlock(appHash, best)
		return appHash, nil
	}

	panic(fmt.Sprintf("uncovered case! appHeight: %d, storeHeight: %d",
		appBlockHeight, storeBlockHeight))
}

func (h *Handshaker) replayBlocks(
	ctx context.Context,
	proxyApp proxy.AppConns,
	appBlockHeight,
	storeBlockHeight int64,
	mutateState bool,
) ([]byte, error) {
	// App is further behind than it should be, so we need to replay blocks.
	// We replay all blocks from appBlockHeight+1.
	//
	// Note that we don't have an old version of the state,
	// so we by-pass state validation/mutation using sm.ExecCommitBlock.
	// This also means we won't be saving validator sets if they change during this period.
	// TODO: Load the historical information to fix this and just use state.ApplyBlock
	//
	// If mutateState == true, the final block is replayed with h.replayBlock()

	var appHash []byte
	var err error
	finalBlock := storeBlockHeight
	if mutateState {
		finalBlock--
	}
	firstBlock := appBlockHeight + 1

	for i := firstBlock; i <= finalBlock; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		h.logger.Info("Applying block", "height", i)
		// Assert previous replay result matches previous block's AppHash (block i-1, not i).
		if len(appHash) > 0 && i > firstBlock {
			prevBlock, _ := h.chain.GetTrunkBlock(uint32(i - 1))
			assertAppHashEqualsOneFromBlock(appHash, prevBlock)
		}

		appHash, _, err = h.replayBlock(int64(i), proxyApp.Consensus())
		if err != nil {
			return nil, err
		}

		h.nBlocks++
	}

	return appHash, nil
}

// ApplyBlock on the proxyApp with the last block.
func (h *Handshaker) replayBlock(height int64, proxyApp proxy.AppConnConsensus) ([]byte, *cmttypes.ValidatorSet, error) {
	block, err := h.chain.GetTrunkBlock(uint32(height))
	if err != nil {
		return nil, nil, err
	}

	// Use stubs for both mempool and evidence pool since no transactions nor
	// evidence are needed here - block already exists.
	blockExec := NewExecutor(proxyApp, h.chain, h.txPool)
	blockExec.SetEventBus(h.eventBus)

	appHash, nxtVSet, err := blockExec.ApplyBlock(block, int64(block.Number()))
	if err != nil {
		return appHash, nxtVSet, err
	}
	// nBlocks is incremented by the caller (replayBlocks) per replayed block to avoid double counting
	return appHash, nxtVSet, nil
}

func assertAppHashEqualsOneFromBlock(appHash []byte, blk *block.Block) {
	expected := blk.AppHash()
	// Skip assertion for legacy blocks that were never persisted with AppHash (e.g. before UpdateBlockAppHash fix).
	if len(expected) == 0 {
		return
	}
	if !bytes.Equal(appHash, expected) {
		panic(fmt.Sprintf(`block.AppHash does not match AppHash after replay. Got %X, expected %X.

Block: %v
`,
			appHash, expected, blk))
	}
}
