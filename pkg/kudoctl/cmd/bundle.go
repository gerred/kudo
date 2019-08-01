package cmd

import (
	"fmt"
	"io"

	"github.com/kudobuilder/kudo/pkg/kudoctl/bundle"
	"github.com/spf13/cobra"
)

const (
	example = `
		The bundle argument must be a  directory which contains the operator definition files.  The bundle command will create are tgz file containing the operator.

		# Bundle zookeeper (where zookeeper is a folder in the current directory)
		kubectl kudo bundle zookeeper

		# Specify an destination folder other than current working directory
		kubectl kudo bundle ../operators/repository/zookeeper/operator/ --destination=<target_folder>`
)

type bundleCmd struct {
	path        string
	destination string
	overwrite   bool
	out         io.Writer
}

// newBundleCmd creates an operator bundle
func newBundleCmd(out io.Writer) *cobra.Command {

	b := &bundleCmd{out: out}
	cmd := &cobra.Command{
		Use:     "bundle <operator_dir>",
		Short:   "Bundle an official KUDO operator.",
		Long:    `Bundle a KUDO operator from local filesystem.`,
		Example: example,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validate(args); err != nil {
				return err
			}
			b.path = args[0]
			if err := b.run(); err != nil {
				return err
			}
			return nil
		},
		SilenceUsage: true,
	}

	f := cmd.Flags()
	f.StringVarP(&b.destination, "destination", "d", ".", "Location to write the bundle.")
	f.BoolVarP(&b.overwrite, "overwrite", "o", false, "Overwrite existing bundle.")
	return cmd
}

func validate(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expecting exactly one argument - directory of the operator to bundle")
	}
	return nil
}

// run returns the errors associated with cmd env
func (b *bundleCmd) run() error {
	tarfile, err := bundle.ToTarBundle(b.path, b.destination, b.overwrite)
	if err == nil {
		fmt.Fprintf(b.out, "Bundle created: %v\n", tarfile)
	}
	return err
}