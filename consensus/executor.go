package consensus

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v2 "github.com/cometbft/cometbft/api/cometbft/abci/v2"
	abci "github.com/cometbft/cometbft/v2/abci/types"
	abcitypes "github.com/cometbft/cometbft/v2/abci/types"
	cmtcrypto "github.com/cometbft/cometbft/v2/crypto"
	cryptoencoding "github.com/cometbft/cometbft/v2/crypto/encoding"
	cmtproxy "github.com/cometbft/cometbft/v2/proxy"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/chain"
	cmn "github.com/meterio/supernova/libs/common"
	"github.com/meterio/supernova/txpool"
	"github.com/meterio/supernova/types"
)

var (
	ErrInvalidBlock = errors.New("invalid block")
)

type Executor struct {
	proxyApp cmtproxy.AppConnConsensus
	chain    *chain.Chain
	txPool   *txpool.TxPool
	logger   *slog.Logger
	eventBus cmttypes.BlockEventPublisher
}

func NewExecutor(proxyApp cmtproxy.AppConnConsensus, c *chain.Chain, txPool *txpool.TxPool) *Executor {
	return &Executor{proxyApp: proxyApp, chain: c, txPool: txPool, logger: slog.With("pkg", "exec")}
}

func (e *Executor) InitChain(req *abcitypes.InitChainRequest) (*abcitypes.InitChainResponse, error) {
	return e.proxyApp.InitChain(context.TODO(), req)
}

func (e *Executor) PrepareProposal(parent *block.DraftBlock, proposerIndex int, round int32, commitInfo *v2.ExtendedCommitInfo) (*abcitypes.PrepareProposalResponse, error) {
	maxBytes := int64(cmttypes.MaxBlockSizeBytes)

	evSize := int64(0)
	vset := e.chain.GetValidatorsByHash(parent.ProposedBlock.NextValidatorsHash())
	if vset == nil {
		return nil, fmt.Errorf("validator set is nil for hash %x", parent.ProposedBlock.NextValidatorsHash())
	}
	maxDataBytes := cmttypes.MaxDataBytes(maxBytes, evSize, vset.Size())
	proposerAddr, validator := vset.GetByIndex(int32(proposerIndex))

	var lastCommitVotes []v2.ExtendedVoteInfo
	if validator != nil {
		lastCommitVotes = []v2.ExtendedVoteInfo{{Validator: cmttypes.TM2PB.Validator(validator)}}
	}

	executables := e.txPool.Executables()
	txs := make([][]byte, 0)
	for _, tx := range executables {
		txs = append(txs, tx)
	}

	return e.proxyApp.PrepareProposal(context.TODO(), &v2.PrepareProposalRequest{
		MaxTxBytes:         maxDataBytes,
		Txs:                txs,
		LocalLastCommit:    v2.ExtendedCommitInfo{Round: round, Votes: lastCommitVotes},
		Misbehavior:        make([]v2.Misbehavior, 0), // FIXME: track the misbehavior and preppare the evidence
		Height:             int64(parent.Height) + 1,
		Time:               time.Now(),
		NextValidatorsHash: parent.ProposedBlock.NextValidatorsHash(),
		ProposerAddress:    proposerAddr,
	})
}

func (e *Executor) ProcessProposal(blk *block.Block) (bool, error) {
	vset := e.chain.GetValidatorsByHash(blk.ValidatorsHash())
	parent, err := e.chain.GetBlock(blk.ParentID())
	if err != nil {
		parentDraft := e.chain.GetDraft(blk.ParentID())
		if parentDraft == nil {
			return false, fmt.Errorf("parent block not found for proposal (parent=%x)", blk.ParentID().Bytes())
		}
		parent = parentDraft.ProposedBlock
	}
	proposerAddr, _ := vset.GetByIndex(int32(blk.ProposerIndex()))
	resp, err := e.proxyApp.ProcessProposal(context.TODO(), &v2.ProcessProposalRequest{
		Hash:               blk.ID().Bytes(),
		Height:             int64(blk.Number()),
		Time:               time.Unix(0, int64(blk.NanoTimestamp())),
		Txs:                blk.Txs.Convert(),
		ProposedLastCommit: e.chain.BuildLastCommitInfo(parent, blk),
		Misbehavior:        make([]v2.Misbehavior, 0), // FIXME: track the misbehavior and preppare the evidence
		ProposerAddress:    proposerAddr,
		NextValidatorsHash: blk.NextValidatorsHash(),
	})

	if err != nil {
		return false, err
	}
	if resp.IsStatusUnknown() {
		panic("ProcessProposal responded with status " + resp.Status.String())
	}

	// 不在 ProcessProposal 接受时移除交易：提案可能未获 2/3 投票或超时，仅在实际提交后移除（见 applyBlock）
	return resp.IsAccepted(), nil
}

func (e *Executor) ExtendVote(req *abcitypes.ExtendVoteRequest) (*abcitypes.ExtendVoteResponse, error) {
	return e.proxyApp.ExtendVote(context.TODO(), req)
}

func (e *Executor) VerifyVoteExtension(req *abcitypes.VerifyVoteExtensionRequest) (*abcitypes.VerifyVoteExtensionResponse, error) {
	return e.proxyApp.VerifyVoteExtension(context.TODO(), req)
}

func (e *Executor) FinalizeBlock(req *abcitypes.FinalizeBlockRequest) (*abcitypes.FinalizeBlockResponse, error) {
	return e.proxyApp.FinalizeBlock(context.TODO(), req)
}

func (e *Executor) Commit() (*abcitypes.CommitResponse, error) {
	return e.proxyApp.Commit(context.TODO())
}

func validateBlock(b *block.Block) error {
	// FIXME: imple this
	return nil
}

// ApplyBlock validates the block against the state, executes it against the app,
// fires the relevant events, commits the app, and saves the new state and responses.
// It returns the new state.
// It's the only function that needs to be called
// from outside this package to process and commit an entire block.
// It takes a blockID to avoid recomputing the parts hash.
func (e *Executor) ApplyBlock(block *block.Block, syncingToHeight int64) ([]byte, *cmttypes.ValidatorSet, error) {
	if err := validateBlock(block); err != nil {
		return make([]byte, 0), nil, ErrInvalidBlock
	}

	return e.applyBlock(block, syncingToHeight)
}

func (e *Executor) applyBlock(blk *block.Block, syncingToHeight int64) (appHash []byte, nxtVSet *cmttypes.ValidatorSet, err error) {
	vset := e.chain.GetValidatorsByHash(blk.ValidatorsHash())
	parent, err := e.chain.GetBlock(blk.ParentID())
	if err != nil {
		parentDraft := e.chain.GetDraft(blk.ParentID())
		if parentDraft == nil {
			return nil, nil, fmt.Errorf("parent block not found for apply (parent=%x)", blk.ParentID().Bytes())
		}
		parent = parentDraft.ProposedBlock
	}
	proposerAddr, _ := vset.GetByIndex(int32(blk.ProposerIndex()))
	decidedLastCommit := e.chain.BuildLastCommitInfo(parent, blk)
	// BaseApp optimistic execution keys off the last ProcessProposal hash. Some consensus paths
	// can finalize without a matching ProcessProposal pass; always run ProcessProposal immediately
	// before FinalizeBlock so OE hash matches (see also pacemaker ValidateProposal).
	accepted, perr := e.ProcessProposal(blk)
	if perr != nil {
		return nil, nil, perr
	}
	if !accepted {
		return nil, nil, fmt.Errorf("ProcessProposal rejected block #%d before FinalizeBlock", blk.Number())
	}
	abciResponse, err := e.proxyApp.FinalizeBlock(context.TODO(), &abci.FinalizeBlockRequest{
		Hash:               blk.ID().Bytes(),
		NextValidatorsHash: blk.Header().NextValidatorsHash,
		ProposerAddress:    proposerAddr,
		Height:             int64(blk.Number()),
		Time:               time.Unix(0, int64(blk.NanoTimestamp())),
		DecidedLastCommit:  decidedLastCommit,
		Misbehavior:        make([]v2.Misbehavior, 0), // FIXME: track the misbehavior and preppare the evidence
		Txs:                blk.Transactions().Convert(),
		SyncingToHeight:    syncingToHeight,
	})
	if err != nil {
		e.logger.Error("Finalize block failed", "err", err)
		return
	}
	appHash = abciResponse.AppHash
	e.logger.Info(
		"Finalized block",
		"height", blk.Number(),
		"num_txs_res", len(abciResponse.TxResults),
		"num_val_updates", len(abciResponse.ValidatorUpdates),
		"block_app_hash", fmt.Sprintf("%X", abciResponse.AppHash),
		"syncing_to_height", syncingToHeight,
	)
	_, err = e.proxyApp.Commit(context.TODO())
	if err != nil {
		e.logger.Error("Commit failed", "err", err)
	}

	// Assert that the application correctly returned tx results for each of the transactions provided in the block
	if len(blk.Txs) != len(abciResponse.TxResults) {
		err = fmt.Errorf("expected tx results length to match size of transactions in block. Expected %d, got %d", len(blk.Txs), len(abciResponse.TxResults))
		return
	}

	// 仅在区块实际提交后从交易池移除，避免 ProcessProposal 接受但未形成 QC 时误删导致交易无法被重新提议
	for _, tx := range blk.Txs {
		e.txPool.Remove(tx.Hash())
	}

	for index, txResult := range abciResponse.TxResults {
		e.logger.Info("tx result", "index", index, "code", txResult.Code, "log", txResult.Log, "info", txResult.Info)
	}

	// calculate the next committee
	if len(abciResponse.ValidatorUpdates) > 0 {
		e.logger.Info("block has validator updates", "len", len(abciResponse.ValidatorUpdates))
		curVSet := e.chain.GetValidatorsByHash(blk.ValidatorsHash())
		e.logger.Info("current validator set", "len", len(curVSet.Validators), "hash", hex.EncodeToString(curVSet.Hash()))
		var calcErr error
		nxtVSet, calcErr = calcNewValidatorSet(curVSet, abciResponse.ValidatorUpdates, abciResponse.Events)
		if calcErr != nil {
			e.logger.Error("calc new validator set failed", "err", calcErr)
			return nil, nil, calcErr
		}
		e.logger.Info("next validator set", "len", len(nxtVSet.Validators), "hash", hex.EncodeToString(nxtVSet.Hash()))
	} else {
		nxtVSet = nil
	}

	return
}

// normalizeValidatorPubKeyType 将应用层使用的公钥类型名映射为 CometBFT codec 接受的 KeyType（如 "bls12-381.pubkey" -> "bls12_381"）。
func normalizeValidatorPubKeyType(pubKeyType string) string {
	switch pubKeyType {
	case "bls12-381.pubkey", "cometbft/PubKeyBls12_381":
		return "bls12_381"
	}
	return pubKeyType
}

// decodeValidatorPubKey 解析 ValidatorUpdate 中的公钥。支持 BLS 两种格式：48 字节（Prysm/应用）与 96 字节（CometBFT）。
func decodeValidatorPubKey(update abcitypes.ValidatorUpdate) (cmtcrypto.PubKey, error) {
	pkType := normalizeValidatorPubKeyType(update.PubKeyType)
	// 应用可能返回 48 字节 BLS（Prysm 格式），CometBFT codec 仅支持 96 字节，此处用 types.BLSPubKey 兼容 48 字节
	if pkType == "bls12_381" && len(update.PubKeyBytes) == 48 {
		return types.BLSPubKey(update.PubKeyBytes), nil
	}
	return cryptoencoding.PubKeyFromTypeAndBytes(pkType, update.PubKeyBytes)
}

func calcNewValidatorSet(vset *cmttypes.ValidatorSet, updates abcitypes.ValidatorUpdates, events []abcitypes.Event) (nxtVSet *cmttypes.ValidatorSet, err error) {
	if updates.Len() <= 0 {
		return nil, nil
	}
	nxtVSetAdapter := cmn.NewValidatorSetAdapter(vset)

	for _, update := range updates {
		pubkey, err := decodeValidatorPubKey(update)
		if err != nil {
			return nil, fmt.Errorf("validator update pubkey decode failed (type=%q): %w", update.PubKeyType, err)
		}
		if update.Power == 0 {
			nxtVSetAdapter.DeleteByPubkey(update.PubKeyBytes)
		} else {
			v := nxtVSetAdapter.GetByPubkey(update.PubKeyBytes)

			if v == nil {
				v = &cmttypes.Validator{PubKey: pubkey, VotingPower: update.Power}
			}

			v.VotingPower = update.Power
			v.Address = pubkey.Address()
			nxtVSetAdapter.Upsert(v)
		}
	}

	nxtVSet = nxtVSetAdapter.ToValidatorSet()
	return nxtVSet, nil
}

func CalcAddedValidators(curVSet, nxtVSet *cmttypes.ValidatorSet) (added []*cmttypes.Validator) {
	if nxtVSet == nil {
		return
	}
	visited := make(map[string]bool)
	for _, v := range curVSet.Validators {
		visited[hex.EncodeToString(v.PubKey.Bytes())] = true
	}
	for _, v := range nxtVSet.Validators {
		if _, exist := visited[hex.EncodeToString(v.PubKey.Bytes())]; !exist {
			added = append(added, v)
		}
	}
	return
}

// SetEventBus - sets the event bus for publishing block related events.
// If not called, it defaults to types.NopEventBus.
func (e *Executor) SetEventBus(eventBus cmttypes.BlockEventPublisher) {
	e.eventBus = eventBus
}
