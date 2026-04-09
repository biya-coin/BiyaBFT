package common

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"

	v2 "github.com/cometbft/cometbft/api/cometbft/abci/v2"
	cmtcrypto "github.com/cometbft/cometbft/v2/crypto"
	cryptoencoding "github.com/cometbft/cometbft/v2/crypto/encoding"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type ValidatorSetAdapter struct {
	Validators []*cmttypes.Validator `json:"validators"`
}

// Volatile state for each Validator
// NOTE: The Accum is not included in Validator.Hash();
// make sure to update that method if changes are made here
type ValidatorSortKey struct {
	PubKey  cmtcrypto.PubKey
	SortKey []byte
}

func NewValidatorSetAdapter(vset *cmttypes.ValidatorSet) *ValidatorSetAdapter {
	vals := &ValidatorSetAdapter{
		Validators: vset.Copy().Validators,
	}
	return vals
}

func (vals *ValidatorSetAdapter) String() string {
	s := make([]string, 0)
	for i, v := range vals.Validators {
		s = append(s, strconv.Itoa(i+1)+v.String())
	}
	return strings.Join(s, "\n")
}

func (vals *ValidatorSetAdapter) Upsert(newV *cmttypes.Validator) {
	for _, v := range vals.Validators {
		if bytes.Equal(v.PubKey.Bytes(), newV.PubKey.Bytes()) {
			v.Address = newV.Address
		}
	}
	vals.Validators = append(vals.Validators, newV)
}

func (vals *ValidatorSetAdapter) GetByPubkey(pubkeyBytes []byte) (val *cmttypes.Validator) {
	for _, v := range vals.Validators {
		if bytes.Equal(v.PubKey.Bytes(), pubkeyBytes) {
			return v
		}
	}
	return nil
}

func (vals *ValidatorSetAdapter) DeleteByPubkey(pubkeyBytes []byte) {
	for index, v := range vals.Validators {
		if bytes.Equal(v.PubKey.Bytes(), pubkeyBytes) {
			vals.Validators = append(vals.Validators[:index], vals.Validators[index+1:]...)
			return
		}
	}
}

func (vals *ValidatorSetAdapter) SortWithNonce(nonce uint64) *cmttypes.ValidatorSet {
	buf := make([]byte, binary.MaxVarintLen64)
	binary.PutUvarint(buf, nonce)

	sortKeys := make([]*ValidatorSortKey, 0)
	for _, d := range vals.Validators {
		sortKeys = append(sortKeys, &ValidatorSortKey{
			PubKey:  d.PubKey,
			SortKey: crypto.Keccak256(append(d.PubKey.Bytes(), buf...)),
		})
	}

	sort.SliceStable(sortKeys, func(i, j int) bool {
		return (bytes.Compare(sortKeys[i].SortKey, sortKeys[j].SortKey) <= 0)
	})

	sorted := make([]*cmttypes.Validator, 0)
	for _, key := range sortKeys {
		sorted = append(sorted, vals.GetByPubkey(key.PubKey.Bytes()))
	}

	sortedVSet := cmttypes.NewValidatorSet(sorted)
	return sortedVSet
}

func (vals *ValidatorSetAdapter) ToValidatorSet() *cmttypes.ValidatorSet {
	return cmttypes.NewValidatorSet(vals.Validators)
}

// pubKeyTypeForDecode normalizes PubKeyType from InitChainResponse (may be empty or Cosmos type URL)
// and infers type from length when empty, so CometBFT cryptoencoding can decode.
func pubKeyTypeForDecode(pubKeyType string, pubKeyBytes []byte) string {
	switch {
	case pubKeyType == "":
		// Persisted InitChainResponse often has empty PubKeyType; infer from length.
		switch len(pubKeyBytes) {
		case 32:
			return "ed25519"
		case 48, 96:
			return "bls12_381"
		default:
			return "ed25519" // best-effort for 32-byte legacy
		}
	case strings.Contains(pubKeyType, "ed25519") || pubKeyType == "tendermint/PubKeyEd25519":
		return "ed25519"
	case strings.Contains(pubKeyType, "bls12") || pubKeyType == "cometbft/PubKeyBls12_381":
		return "bls12_381"
	}
	return pubKeyType
}

func ApplyUpdatesToValidatorSet(vset *cmttypes.ValidatorSet, validatorUpdates []v2.ValidatorUpdate) *cmttypes.ValidatorSet {
	vs := vset.Validators

	addedIndex := make(map[int]bool)
	for i := range validatorUpdates {
		addedIndex[i] = true
	}

	// delete/update existing validator
	for i := 0; i < len(vs); {
		v := vs[i]
		matched := false
		for j, update := range validatorUpdates {
			if bytes.Equal(v.PubKey.Bytes(), update.PubKeyBytes) {
				matched = true
				if v.VotingPower == 0 {
					vs = append(vs[:i], vs[i+1:]...)
					delete(addedIndex, j)
					break
				} else {
					vs[i].VotingPower = update.Power
					i++
					delete(addedIndex, j)
					break
				}
			}
		}
		if !matched {
			i++ // no update matched this validator; advance to avoid infinite loop
		}
	}

	// add new validator
	for i := range addedIndex {
		added := validatorUpdates[i]
		pkType := pubKeyTypeForDecode(added.PubKeyType, added.PubKeyBytes)
		pubKey, err := cryptoencoding.PubKeyFromTypeAndBytes(pkType, added.PubKeyBytes)
		if err != nil {
			// CometBFT may only support 96-byte BLS; 48-byte is handled elsewhere. Retry ed25519 for 32-byte.
			if len(added.PubKeyBytes) == 32 {
				pubKey, err = cryptoencoding.PubKeyFromTypeAndBytes("ed25519", added.PubKeyBytes)
			}
		}
		if err != nil {
			fmt.Printf("ApplyUpdatesToValidatorSet: cannot decode validator %d pubkey type=%q (normalized %q) err=%v\n", i, added.PubKeyType, pkType, err)
			return cmttypes.NewValidatorSet(vs)
		}
		vs = append(vs, cmttypes.NewValidator(pubKey, added.Power))
	}

	return cmttypes.NewValidatorSet(vs)
}
