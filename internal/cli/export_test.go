package cli

import "github.com/spf13/cobra"

// LeafCommands lists every command with no subcommands.
func LeafCommands() []string {
	root := (&app{opts: Options{}.withDefaults()}).rootCommand()
	root.InitDefaultHelpCmd()
	var out []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		children := cmd.Commands()
		if len(children) == 0 {
			out = append(out, cmd.CommandPath())
		}
		for _, child := range children {
			walk(child)
		}
	}
	walk(root)
	return out
}
