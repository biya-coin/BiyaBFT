package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cometbft/cometbft/v2/crypto/bls12381"
	"github.com/cometbft/cometbft/v2/privval"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: genkeys <node-dir>")
		os.Exit(1)
	}
	dir := os.Args[1]
	keyFile := dir + "/config/priv_validator_key.json"
	stateFile := dir + "/data/priv_validator_state.json"

	privKey, err := bls12381.GenPrivKey()
	if err != nil {
		panic(err)
	}
	pv := privval.NewFilePV(privKey, keyFile, stateFile)
	pv.Save()

	pubKey := privKey.PubKey()
	type ExportInfo struct {
		Address    string `json:"address"`
		PubKeyHex  string `json:"pub_key_hex"`
		PubKeyType string `json:"pub_key_type"`
	}
	info := ExportInfo{
		Address:    pubKey.Address().String(),
		PubKeyHex:  fmt.Sprintf("%X", pubKey.Bytes()),
		PubKeyType: "bls12-381.pubkey",
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	fmt.Println(string(b))
}
