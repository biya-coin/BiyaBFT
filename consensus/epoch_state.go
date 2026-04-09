package consensus

import (
	"bytes"
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

func (es *EpochState) AddQCVote(signerIndex uint32, round uint32, blockID types.Bytes32, sig []byte, voteExtension []byte, voteExtensionSignature []byte, nonRpExtension []byte, nonRpExtensionSignature []byte) (*block.QuorumCert, *v2.ExtendedCommitInfo) {
	v := es.committee.Validators[signerIndex]
	signature, err := bls.SignatureFromBytes(sig)
	if err != nil {
		es.logger.Warn(fmt.Sprintf("invalid signature in QC vote R%v", round), "sig", hex.EncodeToString(sig), "err", err)
		return nil, nil
	}
	cmnPubKey, err := cmn.PublicKeyFromBytes(v.PubKey.Bytes())
	if err != nil {
		es.logger.Warn("Invalid key", "err", err)
		return nil, nil
	}
	verified := signature.Verify(cmnPubKey, blockID[:])
	if !verified {
		es.logger.Warn("invalid vote", "sig", hex.EncodeToString(sig), "signerIndex", signerIndex)
		return nil, nil
	}

	extSig, _ := bls.SignatureFromBytes(voteExtensionSignature)
	extMsgHash := types.GetMsgHashForVoteExtension(es.epoch, blockID[:], voteExtension)
	extSigVerified := extSig.Verify(cmnPubKey, extMsgHash)
	if !extSigVerified {
		es.logger.Warn("invalid  vote extension", "sig", hex.EncodeToString(sig), "signerIndex", signerIndex)
		return nil, nil
	}

	nonRpExtSig, _ := bls.SignatureFromBytes(nonRpExtensionSignature)
	nonRpSigVerified := nonRpExtSig.Verify(cmnPubKey, nonRpExtension)
	if !nonRpSigVerified {
		es.logger.Warn("invalid non-rp vote extension", "sig", hex.EncodeToString(sig), "signerIndex", signerIndex)
		return nil, nil
	}

	return es.qcVoteManager.AddVerifiedVote(signerIndex, v, es.epoch, round, blockID, signature, voteExtension, voteExtensionSignature, nonRpExtension, nonRpExtensionSignature)
}

func (es *EpochState) AddTCVote(signerIndex uint32, round uint32, sig []byte, hash [32]byte) *types.TimeoutCert {
	v := es.committee.Validators[signerIndex]
	signature, err := bls.SignatureFromBytes(sig)
	if err != nil {
		es.logger.Warn(fmt.Sprintf("invalid signature in TC vote R%v", round), "sig", hex.EncodeToString(sig), "err", err)
		return nil
	}
	cmnPubKey, err := cmn.PublicKeyFromBytes(v.PubKey.Bytes())
	if err != nil {
		es.logger.Warn("Invalid key", "err", err)
		return nil
	}
	verified := signature.Verify(cmnPubKey, hash[:])
	if !verified {
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

// get the specific round proposer
func (es *EpochState) getRoundProposer(round uint32) *cmttypes.Validator {
	size := int(es.CommitteeSize())
	if size == 0 {
		es.logger.Info("can't get round proposer", "size", size, "round", round)
		return &cmttypes.Validator{}
	}
	_, v := es.committee.GetByIndex(int32(int(round) % size))
	return v
}
