// Copyright (c) 2020 The Meter.io developers
// Package genesis provides AppGenesis-compatible genesis loading for Cosmos-SDK integration.
// When genesis.json is in Cosmos AppGenesis format (has "app_name"), it is parsed and
// converted to CometBFT GenesisDoc with the same checksum semantics as cosmos-sdk server.

package genesis

import (
	"encoding/base64"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	cmtcfg "github.com/cometbft/cometbft/v2/config"
	cmtcrypto "github.com/cometbft/cometbft/v2/crypto"
	cmted25519 "github.com/cometbft/cometbft/v2/crypto/ed25519"
	cmtnode "github.com/cometbft/cometbft/v2/node"
	cmttypes "github.com/cometbft/cometbft/v2/types"
)

// stringOrInt64 unmarshals from JSON string or number (Cosmos genesis often uses strings).
type stringOrInt64 int64

func (s *stringOrInt64) UnmarshalJSON(data []byte) error {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return err
		}
		*s = stringOrInt64(n)
		return nil
	case float64:
		*s = stringOrInt64(x)
		return nil
	default:
		return fmt.Errorf("expected string or number, got %T", v)
	}
}

// appGenesisConsensusParams mirrors consensus params with string-or-int fields for Cosmos JSON.
type appGenesisConsensusParams struct {
	Block    *struct {
		MaxBytes stringOrInt64 `json:"max_bytes"`
		MaxGas   stringOrInt64 `json:"max_gas"`
	} `json:"block,omitempty"`
	Evidence *struct {
		MaxAgeNumBlocks stringOrInt64 `json:"max_age_num_blocks"`
		MaxAgeDuration  stringOrInt64 `json:"max_age_duration"`
		MaxBytes        stringOrInt64 `json:"max_bytes"`
	} `json:"evidence,omitempty"`
	Validator *struct {
		PubKeyTypes []string `json:"pub_key_types,omitempty"`
	} `json:"validator,omitempty"`
}

// appGenesisValidator mirrors GenesisValidator with Power as string (Cosmos exports "power":"1").
type appGenesisValidator struct {
	Address cmttypes.Address `json:"address"`
	PubKey  cmtcrypto.PubKey `json:"pub_key"`
	Power   stringOrInt64   `json:"power"`
	Name    string          `json:"name"`
}

func (v *appGenesisValidator) toGenesisValidator() cmttypes.GenesisValidator {
	return cmttypes.GenesisValidator{
		Address: v.Address,
		PubKey:  v.PubKey,
		Power:   int64(v.Power),
		Name:    v.Name,
	}
}

// appGenesisJSON is a minimal mirror of Cosmos-SDK genutil AppGenesis for JSON parsing only.
type appGenesisJSON struct {
	AppName       string          `json:"app_name"`
	GenesisTime   string          `json:"genesis_time"`
	ChainID       string          `json:"chain_id"`
	InitialHeight int64           `json:"initial_height"`
	AppHash       []byte          `json:"app_hash"`
	AppState      json.RawMessage `json:"app_state,omitempty"`
	Consensus     *struct {
		Validators []appGenesisValidator   `json:"validators,omitempty"`
		Params     *appGenesisConsensusParams `json:"params,omitempty"`
	} `json:"consensus,omitempty"`
}

// genutilState is a minimal view of app_state.genutil for parsing gen_txs.
type genutilState struct {
	GenTxs []json.RawMessage `json:"gen_txs,omitempty"`
}

// genTxMsgCreateValidator is the first message in a gentx body (cosmos.staking.MsgCreateValidator).
type genTxMsgCreateValidator struct {
	PubKey struct {
		Type string `json:"@type"`
		Key  string `json:"key"` // base64
	} `json:"pubkey"`
	Value struct {
		Amount string `json:"amount"`
		Denom  string `json:"denom"`
	} `json:"value"`
	Description struct {
		Moniker string `json:"moniker"`
	} `json:"description"`
}

// genTxBody wraps body.messages[0].
type genTxBody struct {
	Messages []genTxMsgCreateValidator `json:"messages"`
}

// genTxWire is one element of gen_txs array.
type genTxWire struct {
	Body genTxBody `json:"body"`
}

// cosmosDefaultPowerReduction matches sdk.DefaultPowerReduction (10^6); consensus power = tokens / reduction.
const cosmosDefaultPowerReduction = 1_000_000

// validatorsFromGenTxs parses app_state.genutil.gen_txs and returns CometBFT GenesisValidators.
// Used when Cosmos genesis has no consensus.validators (e.g. after collect-gentxs).
// Power is set to tokens/cosmosDefaultPowerReduction so it matches InitChain response (baseapp compares req vs res).
func validatorsFromGenTxs(appState json.RawMessage) ([]cmttypes.GenesisValidator, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(appState, &state); err != nil {
		return nil, err
	}
	genutilRaw, ok := state["genutil"]
	if !ok || len(genutilRaw) == 0 {
		return nil, fmt.Errorf("no genutil in app_state")
	}
	var genutil genutilState
	if err := json.Unmarshal(genutilRaw, &genutil); err != nil {
		return nil, err
	}
	if len(genutil.GenTxs) == 0 {
		return nil, fmt.Errorf("gen_txs empty")
	}
	out := make([]cmttypes.GenesisValidator, 0, len(genutil.GenTxs))
	for i, raw := range genutil.GenTxs {
		var tx genTxWire
		if err := json.Unmarshal(raw, &tx); err != nil {
			continue
		}
		if len(tx.Body.Messages) == 0 {
			continue
		}
		msg := &tx.Body.Messages[0]
		if msg.PubKey.Key == "" {
			continue
		}
		keyBz, err := base64.StdEncoding.DecodeString(msg.PubKey.Key)
		if err != nil || len(keyBz) != 32 {
			continue
		}
		pk := cmted25519.PubKey(keyBz)
		// Cosmos uses consensus power = tokens / DefaultPowerReduction (1e6); must match InitChain response.
		power := int64(1)
		if msg.Value.Amount != "" {
			tokens, err := strconv.ParseInt(msg.Value.Amount, 10, 64)
			if err == nil && tokens > 0 {
				power = tokens / cosmosDefaultPowerReduction
				if power < 1 {
					power = 1
				}
			}
		}
		name := msg.Description.Moniker
		if name == "" {
			name = fmt.Sprintf("validator-%d", i)
		}
		out = append(out, cmttypes.GenesisValidator{
			Address: pk.Address(),
			PubKey:  pk,
			Power:   power,
			Name:    name,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no validators from gen_txs")
	}
	return out, nil
}

// appGenesisParamsToCmt converts Cosmos-style consensus params (string numbers) to CometBFT type.
func appGenesisParamsToCmt(p *appGenesisConsensusParams) *cmttypes.ConsensusParams {
	if p == nil {
		return cmttypes.DefaultConsensusParams()
	}
	out := cmttypes.DefaultConsensusParams()
	if p.Block != nil {
		out.Block.MaxBytes = int64(p.Block.MaxBytes)
		out.Block.MaxGas = int64(p.Block.MaxGas)
	}
	if p.Evidence != nil {
		out.Evidence.MaxAgeNumBlocks = int64(p.Evidence.MaxAgeNumBlocks)
		out.Evidence.MaxAgeDuration = time.Duration(p.Evidence.MaxAgeDuration)
		out.Evidence.MaxBytes = int64(p.Evidence.MaxBytes)
	}
	if p.Validator != nil && len(p.Validator.PubKeyTypes) > 0 {
		out.Validator.PubKeyTypes = p.Validator.PubKeyTypes
	}
	return out
}

// AppGenesisDocProvider returns a GenesisDocProvider that reads the genesis file and,
// if it is in Cosmos AppGenesis format (has "app_name"), converts it to GenesisDoc
// with checksum matching cosmos-sdk getGenDocProvider. Otherwise delegates to CometBFT default.
func AppGenesisDocProvider(cfg *cmtcfg.Config) cmtnode.GenesisDocProvider {
	return func() (cmtnode.ChecksummedGenesisDoc, error) {
		path := cfg.GenesisFile()
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return cmtnode.ChecksummedGenesisDoc{}, err
		}

		var peek struct {
			AppName string `json:"app_name"`
		}
		if err := json.Unmarshal(data, &peek); err != nil {
			return cmtnode.ChecksummedGenesisDoc{}, err
		}

		if peek.AppName != "" {
			return parseAppGenesisAndConvert(data)
		}

		return cmtnode.DefaultGenesisDocProviderFunc(cfg)()
	}
}

func parseAppGenesisAndConvert(data []byte) (cmtnode.ChecksummedGenesisDoc, error) {
	var ag appGenesisJSON
	if err := json.Unmarshal(data, &ag); err != nil {
		return cmtnode.ChecksummedGenesisDoc{}, err
	}

	genTime, err := time.Parse(time.RFC3339Nano, ag.GenesisTime)
	if err != nil {
		genTime, err = time.Parse(time.RFC3339, ag.GenesisTime)
	}
	if err != nil {
		return cmtnode.ChecksummedGenesisDoc{}, err
	}

	if ag.Consensus == nil {
		ag.Consensus = &struct {
			Validators []appGenesisValidator      `json:"validators,omitempty"`
			Params     *appGenesisConsensusParams `json:"params,omitempty"`
		}{}
	}
	validators := make([]cmttypes.GenesisValidator, 0, len(ag.Consensus.Validators))
	for i := range ag.Consensus.Validators {
		validators = append(validators, ag.Consensus.Validators[i].toGenesisValidator())
	}
	// Cosmos collect-gentxs does not write consensus.validators; derive from app_state.genutil.gen_txs.
	if len(validators) == 0 && len(ag.AppState) > 0 {
		fromGenTxs, err := validatorsFromGenTxs(ag.AppState)
		if err == nil && len(fromGenTxs) > 0 {
			validators = fromGenTxs
		}
	}
	consensusParams := appGenesisParamsToCmt(ag.Consensus.Params)

	gen := &cmttypes.GenesisDoc{
		GenesisTime:     genTime,
		ChainID:         ag.ChainID,
		InitialHeight:   ag.InitialHeight,
		AppHash:         ag.AppHash,
		AppState:        ag.AppState,
		Validators:      validators,
		ConsensusParams: consensusParams,
	}
	if err := gen.ValidateAndComplete(); err != nil {
		return cmtnode.ChecksummedGenesisDoc{}, err
	}

	// Checksum: same as cosmos-sdk server getGenDocProvider — SHA256(json.Marshal(gen.AppState.MarshalJSON()))
	genbz, err := gen.AppState.MarshalJSON()
	if err != nil {
		return cmtnode.ChecksummedGenesisDoc{}, err
	}
	bz, err := json.Marshal(genbz)
	if err != nil {
		return cmtnode.ChecksummedGenesisDoc{}, err
	}
	sum := sha256.Sum256(bz)

	return cmtnode.ChecksummedGenesisDoc{
		GenesisDoc:     gen,
		Sha256Checksum: sum[:],
	}, nil
}
