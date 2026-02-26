package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cockroachdb/pebble"
	"github.com/cometbft/cometbft/v2/privval"
	"github.com/cometbft/cometbft/v2/proxy"
	cmttypes "github.com/cometbft/cometbft/v2/types"
	"golang.org/x/sync/errgroup"

	cfg "github.com/cometbft/cometbft/v2/config"
	cmtflags "github.com/cometbft/cometbft/v2/libs/cli/flags"
	cmtlog "github.com/cometbft/cometbft/v2/libs/log"
	cmtnode "github.com/cometbft/cometbft/v2/node"
	node "github.com/meterio/supernova/node"

	"github.com/meterio/supernova/types"
	"github.com/spf13/viper"
)

var homeDir string

func init() {
	flag.StringVar(&homeDir, "cmt-home", "", "Path to the CometBFT config directory (if empty, uses $HOME/.supernova)")
}

var (
	logger = cmtlog.NewLogger(os.Stdout).With("module", "priv_val")
)

func main() {

	flag.Parse()
	if homeDir == "" {
		homeDir = os.ExpandEnv("$HOME/.supernova")
	}

	config := cfg.DefaultConfig()
	config.SetRoot(homeDir)
	viper.SetConfigFile(fmt.Sprintf("%s/%s", homeDir, "config/config.toml"))

	if err := viper.ReadInConfig(); err != nil {
		slog.Error("Reading config", "err", err)
	}
	if err := viper.Unmarshal(config); err != nil {
		slog.Error("Decoding config", "err", err)
	}
	if err := config.ValidateBasic(); err != nil {
		slog.Error("Invalid configuration data", "err", err)
	}
	// config.SetRoot sets RootDir but Viper/config keys might already be loaded with defaults
	config.SetRoot(homeDir)

	dbPath := filepath.Join(homeDir, "appdb")
	db, err := pebble.Open(dbPath, &pebble.Options{})

	if err != nil {
		slog.Error("Opening database", "err", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("Closing database", "err", err)
		}
	}()

	// Load genesis doc to extract real validator public keys for the sample app.
	// This ensures ValidatorUpdates in FinalizeBlock use verified genesis keys,
	// not hardcoded test values.
	var genesisValidators []ValidatorInfo
	genDoc, genErr := cmttypes.GenesisDocFromFile(config.GenesisFile())
	if genErr != nil {
		slog.Warn("Could not load genesis doc for ValidatorInfo; sample app will not send ValidatorUpdates", "err", genErr)
	} else {
		genesisValidators = loadValidatorsFromGenesis(genDoc)
		slog.Info("Loaded genesis validators for sample app", "count", len(genesisValidators))
	}

	app := NewKVStoreApplication(db, genesisValidators)

	pv := privval.LoadFilePV(
		config.PrivValidatorKeyFile(),
		config.PrivValidatorStateFile(),
	)

	nodeKey, err := types.LoadNodeKey(config.NodeKeyFile())
	if err != nil {
		slog.Error("failed to load node's key", "nodeKeyFile", config.NodeKeyFile(), "err", err)
	}

	logger := cmtlog.NewLogger(os.Stdout)
	logger, err = cmtflags.ParseLogLevel(config.LogLevel, logger, cfg.DefaultLogLevel)

	if err != nil {
		slog.Error("failed to parse log level", "err", err)
	}
	ctx, cancelFn := context.WithCancel(context.TODO())
	// config.LogLevel = "debug" // default is info
	node, err := node.NewNode(
		ctx,
		config,
		pv,
		nodeKey,
		proxy.NewLocalClientCreator(app),
		cmtnode.DefaultGenesisDocProviderFunc(config),
		cfg.DefaultDBProvider,
		cmtnode.DefaultMetricsProvider(config.Instrumentation),
		logger,
	)

	if err != nil {
		slog.Error("Creating node", "err", err)
	}

	node.Start()
	defer func() {
		cancelFn()
		// node.Stop()
		// node.Wait()
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

// ListenForQuitSignals listens for SIGINT and SIGTERM. When a signal is received,
// the cleanup function is called, indicating the caller can gracefully exit or
// return.
//
// Note, the blocking behavior of this depends on the block argument.
// The caller must ensure the corresponding context derived from the cancelFn is used correctly.
func ListenForQuitSignals(g *errgroup.Group, block bool, cancelFn context.CancelFunc, logger slog.Logger) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	f := func() {
		sig := <-sigCh
		cancelFn()

		logger.Info("caught signal", "signal", sig.String())
	}

	if block {
		g.Go(func() error {
			f()
			return nil
		})
	} else {
		go f()
	}
}
