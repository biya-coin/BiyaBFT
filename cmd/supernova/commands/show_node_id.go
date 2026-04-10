package commands

import (
	"github.com/spf13/cobra"

	"github.com/cometbft/cometbft/v2/p2p"
)

// ShowNodeIDCmd dumps node's ID to the standard output.
// Similar to biyachain's implementation in cosmos-sdk-inj/server/cmt_cmds.go
var ShowNodeIDCmd = &cobra.Command{
	Use:     "show-node-id",
	Aliases: []string{"show_node_id"},
	Short:   "Show this node's ID",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Parse config to get NodeKeyFile path
		cfg, err := ParseConfig(cmd)
		if err != nil {
			return err
		}

		nodeKey, err := p2p.LoadNodeKey(cfg.NodeKeyFile())
		if err != nil {
			return err
		}

		cmd.Println(nodeKey.ID())
		return nil
	},
}
