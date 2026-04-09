package consensus

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	v2 "github.com/cometbft/cometbft/api/cometbft/abci/v2"
	cmtcrypto "github.com/cometbft/cometbft/v2/crypto"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/chain"
	cmn "github.com/meterio/supernova/libs/common"
	"github.com/meterio/supernova/types"

	"github.com/OffchainLabs/prysm/v6/crypto/bls"
	"golang.org/x/crypto/sha3"
)

const (
	// ExcludeWindowSize 冷却窗口：前若干 Round 的 Leader 会被降权（重抽时尽量避开）。
	ExcludeWindowSize = 3
	// MaxRetries 命中冷却名单时的最大重哈希次数（含初次抽签共尝试 MaxRetries+1 次）。
	MaxRetries = 3
)

type EpochState struct {
	logger        *slog.Logger
	epoch         uint64
	startKBlockID types.Bytes32
	// committee calculated from last validator set and last nonce
	committee     *cmttypes.ValidatorSet
	inCommittee   bool
	index         int
	qcVoteManager *QCVoteManager
	tcVoteManager *TCVoteManager
	pending       bool
}

func NewEpochState(c *chain.Chain, leaf *block.Block, myPubKey cmtcrypto.PubKey) (*EpochState, error) {
	logger := slog.With("pkg", "es")
	var kblk *block.Block
	if leaf.IsKBlock() {
		kblk = leaf
	} else {
		var err error
		num := leaf.LastKBlock()
		kblk, err = c.GetTrunkBlock(num)
		if err != nil {
			return nil, err
		}
	}

	vset := c.GetNextValidatorSet(kblk.Number())
	if vset == nil {
		// 重启时 ValidatorSetRegistry.SaveValidatorSet 可能从未将 validator set 写入 BiyaBFT mainDB
		// （block 不是 KBlock 且无 validator update）。从 InitChainResponse 重建 genesis 验证人集。
		logger.Warn("GetNextValidatorSet returned nil, trying InitChainResponse fallback", "kblkNum", kblk.Number())
		initResp, initErr := c.GetInitChainResponse()
		if initResp != nil && len(initResp.Validators) > 0 {
			adapter := &cmn.ValidatorSetAdapter{Validators: make([]*cmttypes.Validator, 0)}
			for _, update := range initResp.Validators {
				pubkey, decErr := decodeValidatorPubKey(update)
				if decErr != nil {
					logger.Warn("skip validator in InitChainResponse", "err", decErr)
					continue
				}
				adapter.Upsert(&cmttypes.Validator{PubKey: pubkey, VotingPower: update.Power})
			}
			vset = adapter.ToValidatorSet()
		}
		if vset == nil || vset.Size() == 0 {
			slog.Error("Could not get next validator set", "num", kblk.Number(), "initErr", initErr)
			return nil, errors.New("could not get next validator set")
		}
		// 将重建的 vset 存入 DB，后续读取可直接从 DB 获取
		c.SaveValidatorSet(vset)
		logger.Info("rebuilt validator set from InitChainResponse saved to DB", "size", vset.Size())
	}
	vsetAdapter := cmn.NewValidatorSetAdapter(vset)
	vsetAdapter.SortWithNonce(kblk.Nonce())
	committee := vsetAdapter.ToValidatorSet()
	logger.Info("calc epoch state", "kblk", kblk.Number(), "vsetHash", hex.EncodeToString(vset.Hash()), "committeeSize", committee.Size())

	// it is used for temp calculate committee set by a given nonce in the fly.
	// also return the committee

	inCommittee := false
	index := -1
	if len(committee.Validators) > 0 {
		for i, val := range committee.Validators {
			if bytes.Equal(val.PubKey.Bytes(), myPubKey.Bytes()) {
				index = i
				inCommittee = true
			}
		}
	}

	curEpochGauge.Set(float64(kblk.Epoch()) + 1)
	return &EpochState{
		epoch:  kblk.Epoch() + 1,
		logger: logger,

		startKBlockID: kblk.ID(),
		committee:     committee,
		inCommittee:   inCommittee,
		index:         index,
		qcVoteManager: NewQCVoteManager(uint32(committee.Size())),
		tcVoteManager: NewTCVoteManager(uint32(committee.Size())),
		pending:       false,
	}, nil
}

func NewPendingEpochState(vset *cmttypes.ValidatorSet, myPubKey bls.PublicKey, curEpoch uint64) (*EpochState, error) {
	logger := slog.With("pkg", "es")
	if vset == nil {
		slog.Error("validator set is nil")
		return nil, errors.New("validator set is nil")
	}
	committee := vset
	logger.Info("calc epoch state", "vsetHash", hex.EncodeToString(vset.Hash()), "committeeSize", committee.Size())

	// it is used for temp calculate committee set by a given nonce in the fly.
	// also return the committee

	inCommittee := false
	index := -1
	if len(committee.Validators) > 0 {
		for i, val := range committee.Validators {
			if bytes.Equal(val.PubKey.Bytes(), myPubKey.Marshal()) {
				index = i
				inCommittee = true
			}
		}
	}

	return &EpochState{
		logger: logger,
		epoch:  curEpoch + 1,

		committee:   committee,
		inCommittee: inCommittee,
		index:       index,
		pending:     true,
	}, nil
}

// BLS12-381 signature size (uncompressed) expected by Prysm/blst.
const blsSignatureLen = 96

// verifyConsensusBLSSignature checks vote / timeout signatures against the validator pubkey.
// Ed25519 (32-byte pub): Prysm verify + libs/common derived BLS (matches SignVoteWithDerivedBLS).
// Native bls12_381 (48-byte pub): CometBFT PubKey.VerifySignature (Ethereum BLS + DST).
func verifyConsensusBLSSignature(pub cmtcrypto.PubKey, msg, sig []byte) bool {
	if len(sig) != blsSignatureLen {
		return false
	}
	pkb := pub.Bytes()
	if len(pkb) == 32 {
		signature, err := bls.SignatureFromBytes(sig)
		if err != nil {
			return false
		}
		cmnPubKey, err := cmn.PublicKeyFromBytes(pkb)
		if err != nil {
			return false
		}
		return signature.Verify(cmnPubKey, msg)
	}
	return pub.VerifySignature(msg, sig)
}

func (es *EpochState) AddQCVote(signerIndex uint32, round uint32, blockID types.Bytes32, sig []byte, voteExtension []byte, voteExtensionSignature []byte, nonRpExtension []byte, nonRpExtensionSignature []byte) (*block.QuorumCert, *v2.ExtendedCommitInfo) {
	if len(sig) != blsSignatureLen {
		// Timeout without last vote sends empty sig; skip silently.
		return nil, nil
	}
	v := es.committee.Validators[signerIndex]
	if !verifyConsensusBLSSignature(v.PubKey, blockID[:], sig) {
		es.logger.Warn("invalid vote", "sig", hex.EncodeToString(sig), "signerIndex", signerIndex)
		return nil, nil
	}
	signature, err := bls.SignatureFromBytes(sig)
	if err != nil {
		es.logger.Warn(fmt.Sprintf("invalid signature in QC vote R%v", round), "sig", hex.EncodeToString(sig), "err", err)
		return nil, nil
	}

	extMsgHash := types.GetMsgHashForVoteExtension(es.epoch, blockID[:], voteExtension)
	if !verifyConsensusBLSSignature(v.PubKey, extMsgHash, voteExtensionSignature) {
		es.logger.Warn("invalid  vote extension", "sig", hex.EncodeToString(sig), "signerIndex", signerIndex)
		return nil, nil
	}

	if !verifyConsensusBLSSignature(v.PubKey, nonRpExtension, nonRpExtensionSignature) {
		es.logger.Warn("invalid non-rp vote extension", "sig", hex.EncodeToString(sig), "signerIndex", signerIndex)
		return nil, nil
	}

	return es.qcVoteManager.AddVerifiedVote(signerIndex, v, es.epoch, round, blockID, signature, voteExtension, voteExtensionSignature, nonRpExtension, nonRpExtensionSignature)
}

func (es *EpochState) AddTCVote(signerIndex uint32, round uint32, sig []byte, hash [32]byte) *types.TimeoutCert {
	if len(sig) != blsSignatureLen {
		return nil
	}
	v := es.committee.Validators[signerIndex]
	if !verifyConsensusBLSSignature(v.PubKey, hash[:], sig) {
		es.logger.Warn("invalid vote", "sig", hex.EncodeToString(sig), "signerIndex", signerIndex)
		return nil
	}
	return es.tcVoteManager.AddVote(signerIndex, es.epoch, round, sig, hash)
}

func (es *EpochState) CommitteeSize() uint32 {
	return uint32(es.committee.Size())
}

func (es *EpochState) InCommittee() bool {
	return es.inCommittee
}

func (es *EpochState) CommitteeIndex() int {
	return es.index
}

func (es *EpochState) GetValidatorByIndex(index int) ([]byte, *cmttypes.Validator) {
	return es.committee.GetByIndex(int32(index))
}

func (es *EpochState) GetMyself() *cmttypes.Validator {
	if es.inCommittee {
		_, v := es.committee.GetByIndex(int32(es.index))
		return v
	}
	return nil
}

func (es *EpochState) PrintCommittee() {
	fmt.Printf("* Current Committee (%d):\n%s\n\n", es.committee.Size(), peekCommittee(es.committee))
}

func peekCommittee(committee *cmttypes.ValidatorSet) string {
	s := make([]string, 0)
	if committee.Size() > 6 {
		for index, val := range committee.Validators[:3] {
			s = append(s, fmt.Sprintf("#%-4v %v", index, val.String()))
		}
		s = append(s, "...")
		for index, val := range committee.Validators[committee.Size()-3:] {
			s = append(s, fmt.Sprintf("#%-4v %v", index+committee.Size()-3, val.String()))
		}
	} else {
		for index, val := range committee.Validators {
			s = append(s, fmt.Sprintf("#%-2v %v", index, val.String()))
		}
	}
	return strings.Join(s, "\n")
}

// getProposerByWeight 在 committee 固定顺序下按 VotingPower 做加权抽签（O(n) 扫描，ticket 为 O(1)）。
func getProposerByWeight(committee *cmttypes.ValidatorSet, seedBytes []byte) *cmttypes.Validator {
	totalPower := committee.TotalVotingPower()
	if totalPower == 0 || len(committee.Validators) == 0 {
		return &cmttypes.Validator{}
	}

	hash := sha3.Sum256(seedBytes)
	randVal := binary.BigEndian.Uint64(hash[:8])
	ticket := int64(randVal % uint64(totalPower))

	var accum int64
	for _, val := range committee.Validators {
		accum += val.VotingPower
		if accum > ticket {
			return val
		}
	}
	return committee.Validators[0]
}

// getSeedForRound 由本 epoch 锚定的 K 块 ID 与 round 构造确定性种子。
func (es *EpochState) getSeedForRound(round uint32) []byte {
	kid := es.startKBlockID.Bytes()
	seedBytes := make([]byte, len(kid)+4)
	copy(seedBytes, kid)
	binary.BigEndian.PutUint32(seedBytes[len(kid):], round)
	return seedBytes
}

// getRoundProposer 带冷却拦截的 Leader 选举：先收集近 ExcludeWindow 轮理论 Leader，再对当前 round 抽签，碰撞则重哈希种子。
func (es *EpochState) getRoundProposer(round uint32) *cmttypes.Validator {
	size := int(es.CommitteeSize())
	if size == 0 {
		es.logger.Info("can't get round proposer", "size", size, "round", round)
		return &cmttypes.Validator{}
	}

	excludeMap := make(map[string]struct{})
	for i := uint32(1); i <= ExcludeWindowSize; i++ {
		if round < i {
			break
		}
		pastSeed := es.getSeedForRound(round - i)
		pastLeader := getProposerByWeight(es.committee, pastSeed)
		excludeMap[string(pastLeader.Address)] = struct{}{}
	}

	currentSeed := es.getSeedForRound(round)
	var selectedLeader *cmttypes.Validator

	for attempt := 0; attempt <= MaxRetries; attempt++ {
		selectedLeader = getProposerByWeight(es.committee, currentSeed)
		_, excluded := excludeMap[string(selectedLeader.Address)]
		if !excluded || attempt == MaxRetries {
			break
		}
		h := sha3.Sum256(currentSeed)
		currentSeed = h[:]
	}

	return selectedLeader
}
