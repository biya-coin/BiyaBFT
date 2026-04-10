package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	cfg "github.com/cometbft/cometbft/v2/config"
	"github.com/cometbft/cometbft/v2/crypto"
	"github.com/cometbft/cometbft/v2/crypto/bls12381"
	"github.com/cometbft/cometbft/v2/crypto/ed25519"
	cmtlog "github.com/cometbft/cometbft/v2/libs/log"
	cmtos "github.com/cometbft/cometbft/v2/libs/os"
	"github.com/cometbft/cometbft/v2/p2p"
	"github.com/cometbft/cometbft/v2/privval"
	"github.com/cometbft/cometbft/v2/types"
	cmttime "github.com/cometbft/cometbft/v2/types/time"
)

var logger = cmtlog.NewLogger(os.Stdout).With("module", "init")

var keyType string

func init() {
	InitFilesCmd.Flags().StringVar(&keyType, "key-type", "bls12_381", "validator key type (bls12_381 or ed25519)")
}

// InitFilesCmd initializes a fresh Supernova instance
var InitFilesCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize Supernova (generates config.toml, genesis.json, node_key.json, priv_validator_key.json)",
	RunE:  initFiles,
}

func initFiles(*cobra.Command, []string) error {
	return initFilesWithConfig(config)
}

func initFilesWithConfig(config *cfg.Config) error {
	// Ensure root directory and config directory exist, and create config.toml
	cfg.EnsureRoot(config.RootDir)

	// Generate private validator files with the specified key type
	privValKeyFile := config.PrivValidatorKeyFile()
	privValStateFile := config.PrivValidatorStateFile()
	var pv *privval.FilePV
	var err error
	if cmtos.FileExists(privValKeyFile) {
		pv = privval.LoadFilePV(privValKeyFile, privValStateFile)
		logger.Info("Found private validator", "keyFile", privValKeyFile,
			"stateFile", privValStateFile)
	} else {
		// Create key generator based on --key-type flag
		var keyGen func() (crypto.PrivKey, error)
		switch keyType {
		case "bls12_381":
			keyGen = func() (crypto.PrivKey, error) {
				return bls12381.GenPrivKey()
			}
		case "ed25519":
			keyGen = func() (crypto.PrivKey, error) {
				return ed25519.GenPrivKey(), nil
			}
		default:
			return fmt.Errorf("unsupported key type: %s (supported: bls12_381, ed25519)", keyType)
		}
		
		pv, err = privval.GenFilePV(privValKeyFile, privValStateFile, keyGen)
		if err != nil {
			return fmt.Errorf("failed to generate private validator: %w", err)
		}
		pv.Save()
		logger.Info("Generated private validator", "keyFile", privValKeyFile,
			"stateFile", privValStateFile, "keyType", keyType)
	}

	// Generate node key
	nodeKeyFile := config.NodeKeyFile()
	if cmtos.FileExists(nodeKeyFile) {
		logger.Info("Found node key", "path", nodeKeyFile)
	} else {
		if _, err := p2p.LoadOrGenNodeKey(nodeKeyFile); err != nil {
			return err
		}
		logger.Info("Generated node key", "path", nodeKeyFile)
	}

	// Generate genesis file
	genFile := config.GenesisFile()
	if cmtos.FileExists(genFile) {
		logger.Info("Found genesis file", "path", genFile)
	} else {
		// BiyaBFT requires numeric chain_id (e.g., "1234" for mainnet, "99999" for testnet)
		// Different from standard CometBFT which accepts string chain_id
		genDoc := types.GenesisDoc{
			ChainID:         "99999",  // Testnet chain ID (numeric string)
			GenesisTime:     cmttime.Now(),
			ConsensusParams: types.DefaultConsensusParams(),
		}
		pubKey, err := pv.GetPubKey()
		if err != nil {
			return fmt.Errorf("can't get pubkey: %w", err)
		}
		genDoc.Validators = []types.GenesisValidator{{
			Address: pubKey.Address(),
			PubKey:  pubKey,
			Power:   10,
		}}
		
		// Set validator key type based on --key-type flag
		if keyType != "" {
			genDoc.ConsensusParams.Validator.PubKeyTypes = []string{keyType}
		}

		if err := genDoc.SaveAs(genFile); err != nil {
			return err
		}
		logger.Info("Generated genesis file", "path", genFile, "keyType", keyType)
	}

	// config.toml is created by cfg.EnsureRoot() above
	logger.Info("Initialized Supernova node", "home", config.RootDir)
	return nil
}
