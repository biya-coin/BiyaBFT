// Copyright (c) 2020 The Meter.io developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package main

import (
	"os"
	"path/filepath"
	"time"

	_ "net/http/pprof"

	"github.com/cometbft/cometbft/v2/cmd/cometbft/commands/debug"
	"github.com/cometbft/cometbft/v2/libs/cli"
	"github.com/spf13/cobra"
	"github.com/meterio/supernova/cmd/supernova/commands"
	"github.com/meterio/supernova/txpool"
)

var (
	DefaultSupernovaDir = ".supernova"
	gitCommit           string
	gitTag              string
	keyStr              string

	defaultTxPoolOptions = txpool.Options{
		Limit:           200000,
		LimitPerAccount: 1024, /*16,*/ //XXX: increase to 1024 from 16 during the testing
		MaxLifetime:     20 * time.Minute,
	}
)

const (
	statePruningBatch = 1024
	indexPruningBatch = 256
	// indexFlatterningBatch = 1024
	GCInterval = 5 * 60 * 1000 // 5 min in millisecond

	blockPruningBatch = 1024
)

func main() {
	rootCmd := commands.RootCmd
	
	// CometBFT subcommands (similar to biyachain's comet subcommand)
	cometCmd := &cobra.Command{
		Use:     "comet",
		Aliases: []string{"cometbft", "tendermint"},
		Short:   "CometBFT subcommands",
	}
	cometCmd.AddCommand(
		commands.ShowNodeIDCmd,
	)
	
	rootCmd.AddCommand(
		commands.RunNodeCmd(),
		commands.InitFilesCmd,
		cometCmd,
		debug.DebugCmd,
		cli.NewCompletionCmd(rootCmd, true),
	)

	cmd := cli.PrepareBaseCmd(rootCmd, "NOVA", os.ExpandEnv(filepath.Join("$HOME", DefaultSupernovaDir)))
	if err := cmd.Execute(); err != nil {
		panic(err)
	}
}
