package consensus

import (
	"fmt"
	"time"

	"github.com/meterio/supernova/block"
	"github.com/meterio/supernova/chain"
	"github.com/meterio/supernova/types"
)

// finalize the block with its own QC
func (p *Pacemaker) CommitBlock(blk *block.Block, escortQC *block.QuorumCert) error {

	start := time.Now()
	p.logger.Debug("try to finalize block", "block", blk.Oneliner())

	best := p.chain.BestBlock()
	if best == nil {
		return fmt.Errorf("chain not initialized (BestBlock is nil)")
	}
	if blk.Number() <= best.Number() {
		return errKnownBlock
	}

	fork, err := p.chain.AddBlock(blk, escortQC)
	if err != nil {
		if err == chain.ErrBlockExist {
			p.logger.Info("block already exist", "id", blk.ID(), "num", blk.Number())
		} else {
			p.logger.Warn("add block failed ...", "err", err, "id", blk.ID(), "num", blk.Number())
		}
		return err
	}

	appHash, nxtVSet, err := p.executor.ApplyBlock(blk, int64(blk.Number())) // TODO: syncingToHeight might need adjustment
	if err != nil {
		return err
	}
	blk.BlockHeader.AppHash = appHash
	if err := p.chain.UpdateBlockAppHash(blk.ID(), appHash); err != nil {
		p.logger.Error("update block AppHash in chain", "err", err, "block", blk.ID())
	}

	if nxtVSet != nil {
		p.logger.Info("next validator set is not empty", "len", len(nxtVSet.Validators))
		p.addedValidators = CalcAddedValidators(p.epochState.committee, nxtVSet)
		p.nextEpochState, err = NewPendingEpochState(nxtVSet, p.blsMaster.PubKey, p.epochState.epoch)
		if err != nil {
			p.logger.Error("could not calc pending epoch state", "err", err)
			return err
		}
		p.logger.Info("register new validator set", "block", blk.Number(), "len", len(nxtVSet.Validators))
		p.validatorSetRegistry.registerNewValidatorSet(blk.Number(), p.epochState.committee, nxtVSet)
		p.logger.Info("next epoch state", "incommittee", p.nextEpochState.inCommittee, "epoch", p.nextEpochState.epoch)
	}

	// unlike processBlock, we do not need to handle fork
	if fork != nil {
		// process fork????
		if len(fork.Branch) > 0 {
			out := fmt.Sprintf("Fork Happened ... fork(Ancestor=%s, Branch=%s), bestBlock=%s", fork.Ancestor.ID().String(), fork.Branch[0].ID().String(), p.chain.BestBlock().ID().String())
			p.logger.Warn(out)
			p.printFork(fork)
			p.scheduleRegulate()
			return ErrForkHappened
		}
	}

	// 仅当本次提交的 QC 对应的区块不低于当前 QCHigh 时才更新，避免用旧区块的 QC（如 E1.R10）覆盖已更新的 QCHigh（如 E2.R0 -> #11）导致后续 OnBeat 时 "Invalid round to propose"
	if p.QCHigh == nil || escortQC.Number() > p.QCHigh.QC.Number() || (escortQC.Number() == p.QCHigh.QC.Number() && escortQC.Round > p.QCHigh.QC.Round) {
		draftBlk := p.chain.GetDraftByEscortQC(escortQC)
		p.QCHigh = &block.DraftQC{QCNode: draftBlk, QC: escortQC}
		p.logger.Info(`QCHigh updated`, "epoch", escortQC.Epoch, "round", escortQC.Round)
	}

	p.logger.Info(fmt.Sprintf("* committed %v", blk.CompactString()), "txs", len(blk.Txs), "epoch", blk.Epoch(), "elapsed", types.PrettyDuration(time.Since(start)))

	p.lastCommitted = blk
	// broadcast the new block to all peers
	// p.communicator.BroadcastBlock(&block.EscortedBlock{Block: blk, EscortQC: escortQC})
	// successfully added the block, update the current hight of consensus

	if blk.IsKBlock() {
		p.logger.Info("committed a KBlock, schedule regulate now", "blk", blk.ID().ToBlockShortID())
		p.scheduleRegulate()
	}

	start = time.Now()
	p.communicator.BroadcastBlock(&block.EscortedBlock{Block: blk, EscortQC: escortQC})
	p.logger.Debug("broadcast elapsed", "elapsed", time.Since(start))

	return nil
}
