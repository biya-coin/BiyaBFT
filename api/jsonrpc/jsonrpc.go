// Copyright (c) 2020 The Meter.io developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package jsonrpc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	cmtabci "github.com/cometbft/cometbft/v2/abci/types"
	cmtproxy "github.com/cometbft/cometbft/v2/proxy"
	ctypes "github.com/cometbft/cometbft/v2/rpc/core/types"
	"github.com/meterio/supernova/chain"
	"github.com/meterio/supernova/txpool"
)

// CheckTx result.
type ResultBroadcastTx struct {
	Code      uint32 `json:"code"`
	Data      []byte `json:"data"`
	Log       string `json:"log"`
	Codespace string `json:"codespace"`

	Hash []byte `json:"hash"`
}

type Result struct {
	JSONRPC string            `json:"jsonrpc"`
	Id      uint32            `json:"id"`
	Result  ResultBroadcastTx `json:"result"`
	Error   string            `json:"error"`
}

const MaxUint32 = 1<<32 - 1

type JSONRPCAPI struct {
	chain         *chain.Chain
	txPool        *txpool.TxPool
	proxyAppQuery cmtproxy.AppConnQuery
	logger        *slog.Logger
}

func New(chain *chain.Chain, txPool *txpool.TxPool, proxyAppQuery cmtproxy.AppConnQuery) *JSONRPCAPI {
	return &JSONRPCAPI{
		chain,
		txPool,
		proxyAppQuery,
		slog.With("api", "query"),
	}
}

func (a *JSONRPCAPI) HandleBroadcastTx(params json.RawMessage) (output json.RawMessage, err error) {
	var m map[string]interface{}
	if err := json.Unmarshal(params, &m); err != nil {
		panic(err)
	}

	txBase64 := m["tx"].(string)
	decodedTx, err := base64.StdEncoding.DecodeString(txBase64)
	if err != nil {
		panic(err)
	}
	a.txPool.Add(decodedTx)
	// Return a valid ResultBroadcastTx so Cosmos client can decode (hash = SHA256 of tx, same as CometBFT).
	hash := sha256.Sum256(decodedTx)
	// CometBFT client (cosmos-sdk) expects result.hash as hex string
	resultMap := map[string]interface{}{
		"code":      0,
		"log":       "",
		"codespace": "",
		"data":      nil,
		"hash":      hex.EncodeToString(hash[:]),
	}
	resultBytes, err := json.Marshal(resultMap)
	if err != nil {
		return nil, err
	}
	return resultBytes, nil
}

// abciQueryHeight decodes JSON-RPC abci_query "height": Comet HTTP client sends numeric height (JSON number -> float64).
func abciQueryHeight(v interface{}) (int64, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return strconv.ParseInt(x, 10, 64)
	case float64:
		return int64(x), nil
	case json.Number:
		return x.Int64()
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	default:
		return 0, fmt.Errorf("unsupported height type %T", v)
	}
}

func abciQueryProve(v interface{}) bool {
	b, ok := v.(bool)
	return ok && b
}

func abciQueryDataHex(v interface{}) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("data must be hex string, got %T", v)
	}
	return hex.DecodeString(s)
}

func (a *JSONRPCAPI) HandleABCIQuery(params json.RawMessage) (output json.RawMessage, err error) {
	var m map[string]interface{}
	if err := json.Unmarshal(params, &m); err != nil {
		panic(err)
	}

	a.logger.Info("handle ABCI query", "path", m["path"], "data", m["data"], "height", m["height"], "prove", m["prove"])
	height, err := abciQueryHeight(m["height"])
	if err != nil {
		return nil, err
	}
	databytes, err := abciQueryDataHex(m["data"])
	if err != nil {
		return nil, err
	}
	path, ok := m["path"].(string)
	if !ok {
		return nil, fmt.Errorf("path must be string, got %T", m["path"])
	}
	resQuery, err := a.proxyAppQuery.Query(context.TODO(), &cmtabci.QueryRequest{
		Path:   path,
		Data:   databytes,
		Height: height,
		Prove:  abciQueryProve(m["prove"]),
	})
	if err != nil {
		a.logger.Error("proxy app failed to process query", "err", err)
		return nil, err
	}

	// fmt.Println("query result: ")
	// fmt.Println("code: ", resQuery.Code)
	// fmt.Println("codespace: ", resQuery.Codespace)
	// fmt.Println("height: ", resQuery.Height)
	// fmt.Println("index: ", resQuery.Index)
	// fmt.Println("key: ", resQuery.Key)
	// fmt.Println("log: ", resQuery.Log)
	// fmt.Println("info: ", resQuery.Info)
	// fmt.Println("proofOps: ", resQuery.ProofOps)
	// fmt.Println("value: ", resQuery.Value)

	result := ctypes.ResultABCIQuery{Response: *resQuery}

	a.logger.Info("query result", "code", result.Response.Code, "log", result.Response.Log, "info", result.Response.Info)

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return resultBytes, nil
}
