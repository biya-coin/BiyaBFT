package main

import (
	"bytes"
	"context"
	"errors"
	"log"

	"github.com/cockroachdb/pebble"
	abcitypes "github.com/cometbft/cometbft/v2/abci/types"
	cmttypes "github.com/cometbft/cometbft/v2/types"
)

// ValidatorInfo holds runtime validator public key info derived from genesis.
type ValidatorInfo struct {
	PubKeyBytes []byte
	Power       int64
	Name        string
}

type KVStoreApplication struct {
	db           *pebble.DB
	onGoingBatch *pebble.Batch
	validators   []ValidatorInfo // loaded from genesis, used for real validator updates
}

var _ abcitypes.Application = (*KVStoreApplication)(nil)

// NewKVStoreApplication creates a new KVStore app.
// genesisValidators should be populated from the genesis doc's validator set.
func NewKVStoreApplication(db *pebble.DB, genesisValidators []ValidatorInfo) *KVStoreApplication {
	return &KVStoreApplication{db: db, validators: genesisValidators}
}

func (app *KVStoreApplication) isValid(tx []byte) uint32 {
	// check format
	parts := bytes.Split(tx, []byte("="))
	if len(parts) != 2 {
		return 1
	}
	return 0
}

func (app *KVStoreApplication) Info(_ context.Context, info *abcitypes.InfoRequest) (*abcitypes.InfoResponse, error) {
	return &abcitypes.InfoResponse{}, nil
}

func (app *KVStoreApplication) Query(_ context.Context, req *abcitypes.QueryRequest) (*abcitypes.QueryResponse, error) {
	resp := abcitypes.QueryResponse{Key: req.Data}

	value, closer, err := app.db.Get(req.Data)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			resp.Log = "key does not exist"
		} else {
			log.Panicf("Error reading database, unable to execute query: %v", err)
		}
		return &resp, nil
	}
	defer closer.Close()

	resp.Log = "exists"
	resp.Value = value
	return &resp, nil
}

func (app *KVStoreApplication) CheckTx(_ context.Context, check *abcitypes.CheckTxRequest) (*abcitypes.CheckTxResponse, error) {
	code := app.isValid(check.Tx)
	return &abcitypes.CheckTxResponse{Code: code}, nil
}

func (app *KVStoreApplication) InitChain(_ context.Context, chain *abcitypes.InitChainRequest) (*abcitypes.InitChainResponse, error) {
	return &abcitypes.InitChainResponse{}, nil
}

func (app *KVStoreApplication) PrepareProposal(_ context.Context, proposal *abcitypes.PrepareProposalRequest) (*abcitypes.PrepareProposalResponse, error) {
	return &abcitypes.PrepareProposalResponse{Txs: proposal.Txs}, nil
}

func (app *KVStoreApplication) ProcessProposal(_ context.Context, proposal *abcitypes.ProcessProposalRequest) (*abcitypes.ProcessProposalResponse, error) {
	return &abcitypes.ProcessProposalResponse{Status: abcitypes.PROCESS_PROPOSAL_STATUS_ACCEPT}, nil
}

func (app *KVStoreApplication) FinalizeBlock(_ context.Context, req *abcitypes.FinalizeBlockRequest) (*abcitypes.FinalizeBlockResponse, error) {
	var txs = make([]*abcitypes.ExecTxResult, len(req.Txs))
	var updates []abcitypes.ValidatorUpdate

	app.onGoingBatch = app.db.NewBatch()
	defer app.onGoingBatch.Close()

	for i, tx := range req.Txs {
		if code := app.isValid(tx); code != 0 {
			log.Printf("Error: invalid transaction index %v", i)
			txs[i] = &abcitypes.ExecTxResult{Code: code}
		} else {
			parts := bytes.SplitN(tx, []byte("="), 2)
			key, value := parts[0], parts[1]
			log.Printf("Adding key %s with value %s", key, value)

			if err := app.onGoingBatch.Set(key, value, pebble.Sync); err != nil {
				log.Panicf("Error writing to database, unable to execute tx: %v", err)
			}

			log.Printf("Successfully added key %s with value %s", key, value)

			txs[i] = &abcitypes.ExecTxResult{
				Code: 0,
				Events: []abcitypes.Event{
					{
						Type: "app",
						Attributes: []abcitypes.EventAttribute{
							{Key: "key", Value: string(key), Index: true},
							{Key: "value", Value: string(value), Index: true},
						},
					},
				},
			}
		}
	}

	// Demonstrate real validator updates using public keys loaded from genesis.
	// At height 10: reduce voting power of the last validator to 5 (using real genesis key).
	// At height 20: restore voting power of the last validator to original (using real genesis key).
	if len(app.validators) > 0 {
		last := app.validators[len(app.validators)-1]
		switch req.Height {
		case 10:
			log.Printf("Height %d: reducing voting power of validator '%s' to 5", req.Height, last.Name)
			updates = append(updates, abcitypes.ValidatorUpdate{
				PubKeyBytes: last.PubKeyBytes,
				PubKeyType:  "bls12-381.pubkey",
				Power:       5,
			})
		case 20:
			log.Printf("Height %d: restoring voting power of validator '%s' to %d", req.Height, last.Name, last.Power)
			updates = append(updates, abcitypes.ValidatorUpdate{
				PubKeyBytes: last.PubKeyBytes,
				PubKeyType:  "bls12-381.pubkey",
				Power:       last.Power,
			})
		}
	}

	if err := app.onGoingBatch.Commit(pebble.Sync); err != nil {
		log.Panicf("Failed to commit batch: %v", err)
	}

	return &abcitypes.FinalizeBlockResponse{
		TxResults:        txs,
		ValidatorUpdates: updates,
	}, nil
}

func (app *KVStoreApplication) Commit(_ context.Context, commit *abcitypes.CommitRequest) (*abcitypes.CommitResponse, error) {
	// PebbleDB batches are committed in FinalizeBlock, so nothing to do here
	return &abcitypes.CommitResponse{}, nil
}

func (app *KVStoreApplication) ListSnapshots(_ context.Context, snapshots *abcitypes.ListSnapshotsRequest) (*abcitypes.ListSnapshotsResponse, error) {
	return &abcitypes.ListSnapshotsResponse{}, nil
}

func (app *KVStoreApplication) OfferSnapshot(_ context.Context, snapshot *abcitypes.OfferSnapshotRequest) (*abcitypes.OfferSnapshotResponse, error) {
	return &abcitypes.OfferSnapshotResponse{}, nil
}

func (app *KVStoreApplication) LoadSnapshotChunk(_ context.Context, chunk *abcitypes.LoadSnapshotChunkRequest) (*abcitypes.LoadSnapshotChunkResponse, error) {
	return &abcitypes.LoadSnapshotChunkResponse{}, nil
}

func (app *KVStoreApplication) ApplySnapshotChunk(_ context.Context, chunk *abcitypes.ApplySnapshotChunkRequest) (*abcitypes.ApplySnapshotChunkResponse, error) {
	return &abcitypes.ApplySnapshotChunkResponse{Result: abcitypes.APPLY_SNAPSHOT_CHUNK_RESULT_ACCEPT}, nil
}

func (app *KVStoreApplication) ExtendVote(_ context.Context, extend *abcitypes.ExtendVoteRequest) (*abcitypes.ExtendVoteResponse, error) {
	return &abcitypes.ExtendVoteResponse{}, nil
}

func (app *KVStoreApplication) VerifyVoteExtension(_ context.Context, verify *abcitypes.VerifyVoteExtensionRequest) (*abcitypes.VerifyVoteExtensionResponse, error) {
	return &abcitypes.VerifyVoteExtensionResponse{}, nil
}

// loadValidatorsFromGenesis extracts validator info (public key bytes + power + name)
// from a CometBFT GenesisDoc. This allows the application to use real, verified
// validator public keys rather than hardcoded test values.
func loadValidatorsFromGenesis(genDoc *cmttypes.GenesisDoc) []ValidatorInfo {
	infos := make([]ValidatorInfo, 0, len(genDoc.Validators))
	for _, v := range genDoc.Validators {
		infos = append(infos, ValidatorInfo{
			PubKeyBytes: v.PubKey.Bytes(),
			Power:       v.Power,
			Name:        v.Name,
		})
	}
	return infos
}