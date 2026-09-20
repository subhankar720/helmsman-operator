package cli

import "github.com/spf13/cobra"

// NewRootCmd builds the root command.
// All subcommands are registered here.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "helmsman-cli",
		Short: "Helmsman Internal Developer Platform CLI",
		Long: `helmsman-cli is the developer-facing tool for the Helmsman IDP.

It generates AppDeployment CRs and opens fleet-repo PRs so developers
never have to touch YAML or know how the platform is structured.`,
	}

	root.AddCommand(newScaffoldCmd())
	return root
}
