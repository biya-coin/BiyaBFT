// Copyright (c) 2020 The Meter.io developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package types

import (
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"io"
	"math/big"

	"github.com/OffchainLabs/prysm/v6/crypto/bls"
	cmtcrypto "github.com/cometbft/cometbft/v2/crypto"
	"github.com/cometbft/cometbft/v2/crypto/bls12381"
	"github.com/ethereum/go-ethereum/common"
)

// bls12_381 scalar field order (curve order for secret key); bytes must be < this for SecretKeyFromBytes.
var blsScalarOrder = new(big.Int).SetBytes([]byte{
	0x73, 0xed, 0xa7, 0x53, 0x29, 0x9d, 0x7d, 0x48,
	0x33, 0x39, 0xd8, 0x08, 0x09, 0xa1, 0xd8, 0x05,
	0x53, 0xbd, 0xa4, 0x02, 0xff, 0xfe, 0x5b, 0xfe,
	0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x01,
})

// seedToBlsScalarBytes reduces seed (32 bytes, big-endian) modulo BLS scalar order and returns 32 bytes
// so that bls.SecretKeyFromBytes accepts them (prysm/blst rejects values >= order).
func seedToBlsScalarBytes(seed []byte) []byte {
	z := new(big.Int).SetBytes(seed)
	z.Mod(z, blsScalarOrder)
	b := z.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

type BlsMaster struct {
	PrivKey    bls.SecretKey //my private key
	PubKey     bls.PublicKey //my public key
	CmtPrivKey cmtcrypto.PrivKey
	CmtPubKey  cmtcrypto.PubKey
}

func NewBlsMasterWithRandKey() *BlsMaster {
	cmtPrivKey, err := bls12381.GenPrivKey()
	if err != nil {
		panic(err)
	}
	cmtPubKey := cmtPrivKey.PubKey()
	return NewBlsMasterWithCometKeys(cmtPrivKey, cmtPubKey)
}

func NewBlsMasterWithCometKeys(cmtPrivKey cmtcrypto.PrivKey, cmtPubKey cmtcrypto.PubKey) *BlsMaster {
	seed := cmtPrivKey.Bytes()
	// BLS expects 32 bytes; CometBFT Ed25519 (from default init) returns 64 bytes.
	if len(seed) != 32 {
		if len(seed) >= 32 {
			seed = seed[:32]
		} else {
			h := sha256.Sum256(seed)
			seed = h[:]
		}
	}
	// SecretKeyFromBytes requires bytes to be a valid BLS scalar (< curve order); raw Ed25519 bytes often exceed it.
	secretBytes := seedToBlsScalarBytes(seed)
	blsPrivKey, err := bls.SecretKeyFromBytes(secretBytes)
	if err != nil {
		panic(err)
	}
	blsPubKey := blsPrivKey.PublicKey()

	bm := &BlsMaster{
		PrivKey:    blsPrivKey,
		PubKey:     blsPubKey,
		CmtPrivKey: cmtPrivKey,
		CmtPubKey:  cmtPubKey,
	}
	validated := bm.ValidateKeyPair()
	if !validated {
		panic("invalid bls secret")
	}
	return bm
}

func (bm *BlsMaster) ValidateKeyPair() bool {
	h := md5.New()

	_, err := io.WriteString(h, "This is a message to be signed and verified by BLS!")
	if err != nil {
		return false
	}
	msg := h.Sum(nil)
	sig := bm.SignMessage(msg)

	return sig.Verify(bm.PubKey, msg)
}

// BLS is implemented by C, memeory need to be freed.
// Signatures also need to be freed but Not here!!!
func (bm *BlsMaster) Destroy() bool {
	return true
}

func (bm *BlsMaster) GetPublicKey() bls.PublicKey {
	return bm.PubKey
}

func (bm *BlsMaster) GetAddress() common.Address {
	return common.BytesToAddress(bm.PubKey.Marshal())
}

// func (bm *BlsMaster) GetPrivateKey() *bls.PrivateKey {
// 	return &bm.PrivKey
// }

// sign the part of msg
func (bm *BlsMaster) SignMessage(msg []byte) bls.Signature {
	sig := bm.PrivKey.Sign(msg)
	return sig
}

func (bm *BlsMaster) SignHash(hash [32]byte) []byte {
	return bm.PrivKey.Sign(hash[:]).Marshal()
}

// SignVoteWithDerivedBLS signs consensus votes / timeout wishes.
// - 32-byte pubkey (Ed25519): derive BLS from SHA256(pub) — must match
// common.PublicKeyFromBytes + epoch_state.Verify for legacy Cosmos setups.
// - 48-byte pubkey (native bls12_381): use CometBFT priv key signing (same as genesis pubkey).
func (bm *BlsMaster) SignVoteWithDerivedBLS(msg []byte) []byte {
	seed := bm.CmtPubKey.Bytes()
	if len(seed) == 32 {
		h := sha256.Sum256(seed)
		secretBytes := seedToBlsScalarBytes(h[:])
		sk, err := bls.SecretKeyFromBytes(secretBytes)
		if err != nil {
			panic(err)
		}
		return sk.Sign(msg).Marshal()
	}
	sig, err := bm.CmtPrivKey.Sign(msg)
	if err != nil {
		panic(err)
	}
	return sig
}

func (bm *BlsMaster) VerifySignature(signature, msgHash, blsPK []byte) (bool, error) {
	var fixedMsgHash [32]byte
	copy(fixedMsgHash[:], msgHash[32:])
	pubkey, err := bls.PublicKeyFromBytes(blsPK)
	if err != nil {
		fmt.Println("pubkey unmarshal failed")
		return false, nil
	}
	return bls.VerifySignature(signature, [32]byte(msgHash), pubkey)
}

// func (bm *BlsMaster) Print() {
// 	fmt.Println("Bls Secret (Hex): ", hex.EncodeToString(bm.PrivKey.Marshal()))
// 	fmt.Println("Bls Secret (B64): ", base64.StdEncoding.EncodeToString(bm.PrivKey.Marshal()))
// 	fmt.Println("Bls Pubkey (Hex): ", hex.EncodeToString(bm.PubKey.Marshal()))
// 	fmt.Println("Bls Pubkey (B64): ", base64.StdEncoding.EncodeToString(bm.PubKey.Marshal()))
// }
