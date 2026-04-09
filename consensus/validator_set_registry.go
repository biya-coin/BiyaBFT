package consensus

import (
	"errors"
	"fmt"
	"log/slog"

	abcitypes "github.com/cometbft/cometbft/v2/abci/types"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/meterio/supernova/chain"
	cmn "github.com/meterio/supernova/libs/common"
	"github.com/meterio/supernova/types"
)

type ValidatorSetRegistry struct {
	CurrentVSet map[uint32]*cmttypes.ValidatorSet
	NextVSet    map[uint32]*cmttypes.ValidatorSet
	Chain       *chain.Chain
	logger      *slog.Logger
}

func NewValidatorSetRegistry(c *chain.Chain) *ValidatorSetRegistry {
	vset := c.GetBestValidatorSet()
	nxtVSet := c.GetBestNextValidatorSet()
	best := c.BestBlock()
	bestNum := best.Number()

	registry := &ValidatorSetRegistry{
		CurrentVSet: make(map[uint32]*cmttypes.ValidatorSet),
		NextVSet:    make(map[uint32]*cmttypes.ValidatorSet),
		Chain:       c,
		logger:      slog.With("pkg", "vreg"),
	}

	if best.IsKBlock() {
		registry.registerNewValidatorSet(bestNum, vset, nxtVSet)
	} else {
		// 重启时 best block 不是 KBlock，nxtVSet 从 DB 可能读不到（第一次 SaveValidatorSet 从未调用）。
		// 从 InitChainResponse 重建 genesis 验证人集，存入 DB，保证 GetNextValidatorSet 可用。
		registry.ensureValidatorSetPersisted()
	}
	return registry
}

// ensureValidatorSetPersisted 确保 genesis 验证人集被保存到 BiyaBFT mainDB。
// 用于解决重启后 GetNextValidatorSet 返回 nil 的问题。
func (vr *ValidatorSetRegistry) ensureValidatorSetPersisted() {
	// 如果 GetBestNextValidatorSet 已经能读到，无需重复存储
	if existing := vr.Chain.GetBestNextValidatorSet(); existing != nil {
		vr.logger.Info("validator set already persisted, skipping", "hash", existing.Hash())
		return
	}

	// 从 InitChainResponse 重建 genesis 验证人集
	initResp, err := vr.Chain.GetInitChainResponse()
	if err != nil || initResp == nil {
		vr.logger.Warn("could not load InitChainResponse, validator set may be unavailable", "err", err)
		return
	}

	vsetAdapter := &cmn.ValidatorSetAdapter{Validators: make([]*cmttypes.Validator, 0)}
	for _, update := range initResp.Validators {
		pubkey, err2 := decodeValidatorPubKey(update)
		if err2 != nil {
			vr.logger.Warn("failed to decode validator pubkey in InitChainResponse", "err", err2)
			continue
		}
		vsetAdapter.Upsert(&cmttypes.Validator{PubKey: pubkey, VotingPower: update.Power})
	}
	rebuilt := vsetAdapter.ToValidatorSet()
	if rebuilt == nil || rebuilt.Size() == 0 {
		vr.logger.Warn("rebuilt validator set from InitChainResponse is empty")
		return
	}
	vr.Chain.SaveValidatorSet(rebuilt)
	vr.logger.Info("persisted genesis validator set from InitChainResponse", "hash", rebuilt.Hash(), "size", rebuilt.Size())
}

func (vr *ValidatorSetRegistry) registerNewValidatorSet(curNum uint32, vset *cmttypes.ValidatorSet, nxtVSet *cmttypes.ValidatorSet) error {
	bestNum := vr.Chain.BestBlock().Number()
	enableNum := curNum + types.NBlockDelayToEnableValidatorSet - 1
	if curNum == 0 {
		enableNum = 0
	}
	if enableNum <= bestNum {
		return errors.New("could not enable validator set before best block")
	}
	if vset != nil {
		vr.CurrentVSet[enableNum] = vset
	}
	if nxtVSet == nil {
		panic("next validator set could not be empty")
	}
	vr.NextVSet[enableNum] = nxtVSet
	vr.Chain.SaveValidatorSet(nxtVSet)
	vr.logger.Info("registered next vset", "hash", nxtVSet.Hash(), "cur", curNum, "num", enableNum, "size", nxtVSet.Size())

	vr.Prune()
	return nil
}

func (vr *ValidatorSetRegistry) Get(num uint32) *cmttypes.ValidatorSet {
	if vset, exist := vr.CurrentVSet[num]; exist {
		return vset
	}
	return nil
}

func (vr *ValidatorSetRegistry) GetNext(num uint32) *cmttypes.ValidatorSet {
	if vset, exist := vr.NextVSet[num]; exist {
		return vset
	}
	return nil
}

type validatorExtra struct {
	Name    string
	Address common.Address
	Pubkey  []byte
	IP      string
	Port    uint64
}

func (vr *ValidatorSetRegistry) Update(num uint32, vset *cmttypes.ValidatorSet, updates abcitypes.ValidatorUpdates, events []abcitypes.Event) (nxtVSet *cmttypes.ValidatorSet, addedValidators []*cmttypes.Validator, err error) {
	if updates.Len() <= 0 {
		return
	}
	nxtVSetAdapter := cmn.NewValidatorSetAdapter(vset)

	for _, update := range updates {
		pubkey, err := decodeValidatorPubKey(update)
		if err != nil {
			return nil, nil, fmt.Errorf("validator update pubkey decode failed (type=%q): %w", update.PubKeyType, err)
		}
		if update.Power == 0 {
			nxtVSetAdapter.DeleteByPubkey(update.PubKeyBytes)
		} else {
			v := nxtVSetAdapter.GetByPubkey(update.PubKeyBytes)

			added := false
			if v == nil {
				added = true
				v = &cmttypes.Validator{PubKey: pubkey, VotingPower: update.Power}
			}

			v.VotingPower = update.Power
			nxtVSetAdapter.Upsert(v)

			if added {
				addedValidators = append(addedValidators, v)
			}
		}
	}
	err = vr.registerNewValidatorSet(num, vset, nxtVSetAdapter.ToValidatorSet())
	return
}

func (vr *ValidatorSetRegistry) Prune() {
	bestNum := vr.Chain.BestBlock().Number()
	for num, _ := range vr.CurrentVSet {
		if num < bestNum {
			delete(vr.CurrentVSet, num)
		}
	}
}
