package common

import (
	"crypto/sha256"
	"errors"
	"math/big"

	"github.com/OffchainLabs/prysm/v6/crypto/bls/common"

	prysmbls "github.com/OffchainLabs/prysm/v6/crypto/bls"
	cmtcrypto "github.com/cometbft/cometbft/v2/crypto"
	blst "github.com/supranational/blst/bindings/go"
)

// BLS12-381 scalar field order; secret key bytes must represent a value < this.
var blsScalarOrder = new(big.Int).SetBytes([]byte{
	0x73, 0xed, 0xa7, 0x53, 0x29, 0x9d, 0x7d, 0x48,
	0x33, 0x39, 0xd8, 0x08, 0x09, 0xa1, 0xd8, 0x05,
	0x53, 0xbd, 0xa4, 0x02, 0xff, 0xfe, 0x5b, 0xfe,
	0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x01,
})

type (
	blstPublicKey          = blst.P1Affine
	blstSignature          = blst.P2Affine
	blstAggregateSignature = blst.P1Aggregate
	blstAggregatePublicKey = blst.P2Aggregate
)

var (
	// ErrDeserialization is returned when deserialization fails.
	ErrDeserialization = errors.New("bls12381: deserialization error")
	// ErrInfinitePubKey is returned when the public key is infinite. It is part
	// of a more comprehensive subgroup check on the key.
	ErrInfinitePubKey = errors.New("bls12381: pubkey is infinite")
)

// PublicKeyFromBytes creates a BLS public key from bytes. For 32-byte keys (Ed25519
// validator set from Cosmos), derives BLS public key deterministically so that
// vote/TC verification works when signers use BlsMaster.SignVoteWithDerivedBLS.
// WARNING: Ed25519-derived BLS is not secure for multi-validator with untrusted peers.
func PublicKeyFromBytes(pubKey []byte) (common.PublicKey, error) {
	if len(pubKey) == 32 {
		return blsPublicKeyFromEd25519PubKey(pubKey)
	}
	pk := new(blstPublicKey).Deserialize(pubKey)
	if pk == nil {
		return nil, ErrDeserialization
	}
	// Subgroup and infinity check
	if !pk.KeyValidate() {
		return nil, ErrInfinitePubKey
	}
	return prysmbls.PublicKeyFromBytes(pk.Compress())
}

// blsPublicKeyFromEd25519PubKey derives a BLS public key from 32-byte Ed25519 pub key
// (seed = SHA256(pub), scalar = seed mod order, BLS_priv = keygen(scalar), return BLS_pub).
// Must match types.BlsMaster.SignVoteWithDerivedBLS derivation.
func blsPublicKeyFromEd25519PubKey(ed25519Pub []byte) (common.PublicKey, error) {
	if len(ed25519Pub) != 32 {
		return nil, ErrDeserialization
	}
	seed := sha256.Sum256(ed25519Pub)
	z := new(big.Int).SetBytes(seed[:])
	z.Mod(z, blsScalarOrder)
	b := z.Bytes()
	scalar := make([]byte, 32)
	copy(scalar[32-len(b):], b)
	sk, err := prysmbls.SecretKeyFromBytes(scalar)
	if err != nil {
		return nil, err
	}
	return sk.PublicKey(), nil
}

func BlsPublicKeyFromCmtPubKey(cmtPubKey cmtcrypto.PubKey) (common.PublicKey, error) {
	b := cmtPubKey.Bytes()
	if len(b) == 32 {
		return blsPublicKeyFromEd25519PubKey(b)
	}
	pk := new(blstPublicKey).Deserialize(b)
	if pk == nil {
		return nil, ErrDeserialization
	}
	if !pk.KeyValidate() {
		return nil, ErrInfinitePubKey
	}
	return prysmbls.PublicKeyFromBytes(pk.Compress())
}

// BlsDstMinPk is the hash-to-curve domain for CometBFT crypto/bls12381 (minimal-pubkey).
// Prysm FastAggregateVerify defaults to RO_POP; Comet signing uses NUL and must match here.
var BlsDstMinPk = []byte("BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_")

// VerifyFastAggregateCometBLS verifies an aggregate built from CometBFT bls12381 individual signatures.
func VerifyFastAggregateCometBLS(aggSigCompressed []byte, pubKeysCompressed [][]byte, msg []byte) bool {
	if len(aggSigCompressed) == 0 || len(msg) == 0 {
		return false
	}
	sig := new(blst.P2Affine).Uncompress(aggSigCompressed)
	if sig == nil {
		sig = new(blst.P2Affine).Deserialize(aggSigCompressed)
	}
	if sig == nil {
		return false
	}
	pks := make([]*blst.P1Affine, 0, len(pubKeysCompressed))
	for _, raw := range pubKeysCompressed {
		// Match PublicKeyFromBytes: Comet PubKey.Bytes() is Serialize(); use Deserialize, not only Uncompress.
		pk := new(blst.P1Affine).Deserialize(raw)
		if pk == nil {
			pk = new(blst.P1Affine).Uncompress(raw)
		}
		if pk == nil || !pk.KeyValidate() {
			return false
		}
		pks = append(pks, pk)
	}
	if len(pks) == 0 {
		return false
	}
	return sig.FastAggregateVerify(true, pks, msg, BlsDstMinPk)
}
