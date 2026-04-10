// Copyright (c) 2020 The Meter.io developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package types

import (
	"github.com/ethereum/go-ethereum/common"
)

// Constants of block chain.
const (
	// --------------------- Epoch --------------------------
	MTR  = byte(0)
	MTRG = byte(1)
	// minimum height for committee relay.

	//  ------------------ Basics ----------------------------
	// BlockIntervalNano: min wall-clock spacing between consecutive blocks (ns), enforced in consensus CreateLeaf.
	// 过小会导致空块极快、日志与链状态暴涨、内存压力大；压测可调 20e6～50e6；生产请按网络与负载评估。
	BlockIntervalNano uint64 = 10_000_000  // 10ms（本地/联调默认；原 2s 见 git 历史）
	BaseTxGas         uint64 = ParamsTxGas // 21000
	TxGas             uint64 = 5000

	// InitialGasLimit was 10 *1000 *100, only accommodates 476 Txs, block size 61k, so change to 200M
	SloadGas       uint64 = 200 // EIP158 gas table
	SstoreSetGas   uint64 = ParamsSstoreSetGas
	SstoreResetGas uint64 = ParamsSstoreResetGas

	NBlockDelayToEnableValidatorSet = 4
)

// Keys of governance params.
var (
	// Keys
	KeyExecutorAddress = BytesToBytes32([]byte("executor"))

	ZeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")
)
