package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	cmdInit "github.com/kudobuilder/kudo/pkg/kudoctl/cmd/init"
	"github.com/kudobuilder/kudo/pkg/kudoctl/kube"
	"github.com/kudobuilder/kudo/pkg/kudoctl/kudohome"
	"github.com/kudobuilder/kudo/pkg/kudoctl/util/repo"

	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const initDesc = `
This command installs KUDO onto your Kubernetes Cluster and sets up local configuration in $KUDO_HOME (default ~/.kudo/).

As with the rest of the KUDO commands, 'kudo init' discovers Kubernetes clusters by reading 
$KUBECONFIG (default '~/.kube/config') and using the default context.

To set up just a local environment, use '--client-only'. That will configure $KUDO_HOME, but not attempt to connect 
to a Kubernetes cluster and install KUDO.

When installing KUDO, 'kudo init' will attempt to install the latest released
version. You can specify an alternative image with '--kudo-image' which is the fully qualified image name replacement 
or '--version' which will replace the version designation on the standard image.

To dump a manifest containing the KUDO deployment YAML, combine the '--dry-run' and '--output=yaml' flags.
`

type initCmd struct {
	out        io.Writer
	fs         afero.Fs
	image      string
	dryRun     bool
	output     string
	version    string
	wait       bool
	timeout    int64
	clientOnly bool
	home       kudohome.Home
	client     *kube.Client
}

func newInitCmd(fs afero.Fs, out io.Writer) *cobra.Command {
	i := &initCmd{fs: fs, out: out}

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize KUDO on both the client and server",
		Long:  initDesc,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return errors.New("this command does not accept arguments")
			}
			if err := i.validate(); err != nil {
				return err
			}
			i.home = Settings.Home
			return i.run()
		},
	}

	f := cmd.Flags()
	f.BoolVarP(&i.clientOnly, "client-only", "c", false, "If set does not install KUDO")
	f.StringVarP(&i.image, "kudo-image", "i", "", "Override KUDO controller image and/or version")
	f.StringVarP(&i.version, "version", "", "", "Override KUDO controller version of the KUDO image")
	f.StringVarP(&i.output, "output", "o", "", "Output format")
	f.BoolVar(&i.dryRun, "dry-run", false, "Do not install local or remote")
	f.BoolVarP(&i.wait, "wait", "w", false, "Block until KUDO manager is running and ready to receive requests")
	f.Int64Var(&i.timeout, "wait-timeout", 300, "Wait timeout to be used")

	return cmd
}

func (initCmd *initCmd) validate() error {
	// we do not allow the setting of image and version!
	if initCmd.image != "" && initCmd.version != "" {
		return errors.New("specify either 'kudo-image' or 'version', not both")
	}
	return nil
}

// run initializes local config and installs KUDO manager to Kubernetes cluster.
func (initCmd *initCmd) run() error {
	opts := cmdInit.NewOptions(initCmd.version)
	// if image provided switch to it.
	if initCmd.image != "" {
		opts.Image = initCmd.image
	}

	//TODO: implement output=yaml|json (define a type for output to constrain)
	//define an Encoder to replace YAMLWriter
	if strings.ToLower(initCmd.output) == "yaml" {
		mans, err := cmdInit.PrereqManifests(opts)
		if err != nil {
			return err
		}

		crd, err := cmdInit.CRDManifests()
		if err != nil {
			return err
		}

		deploy, err := cmdInit.ManagerManifests(opts)
		if err != nil {
			return err
		}

		mans = append(mans, crd...)
		mans = append(mans, deploy...)
		initCmd.YAMLWriter(initCmd.out, mans)
	}

	if initCmd.dryRun {
		return nil
	}

	// initialize client
	if err := initCmd.initialize(); err != nil {
		return fmt.Errorf("error initializing: %s", err)
	}
	fmt.Fprintf(initCmd.out, "$KUDO_HOME has been configured at %s\n", Settings.Home)

	// initialize server
	if !initCmd.clientOnly {
		if initCmd.client == nil {
			client, err := kube.GetKubeClient(Settings.KubeConfig)
			if err != nil {
				return fmt.Errorf("could not get Kubernetes client: %s", err)
			}
			initCmd.client = client
		}

		if err := cmdInit.Install(initCmd.client, opts); err != nil {
			if apierrors.IsAlreadyExists(err) {
				fmt.Fprintln(initCmd.out, "Warning: KUDO is already installed in the cluster.\n"+
					"(Use --client-only to suppress this message)")
				return fmt.Errorf("error installing: %s", err)
			}
		}

		if initCmd.wait {
			finished := cmdInit.WatchKUDOUntilReady(initCmd.client.KubeClient, opts, initCmd.timeout)
			if !finished {
				return errors.New("watch timed out, readiness uncertain")
			}
		}
	}

	return nil
}

// YAMLWriter writes yaml to writer.   Looked into using https://godoc.org/gopkg.in/yaml.v2#NewEncoder which
// looks like a better way, however the omitted JSON elements are encoded which results in a very verbose output.
//TODO: Write a Encoder util which uses the "sigs.k8s.io/yaml" library for marshalling
func (initCmd *initCmd) YAMLWriter(w io.Writer, manifests []string) error {
	for _, manifest := range manifests {
		if _, err := fmt.Fprintln(w, "---"); err != nil {
			return err
		}

		if _, err := fmt.Fprintln(w, manifest); err != nil {
			return err
		}
	}

	// YAML ending document boundary marker
	_, err := fmt.Fprintln(w, "...")
	return err
}

//func initialize(fs afero.Fs, settings env.Settings, out io.Writer) error {
func (initCmd *initCmd) initialize() error {

	if err := ensureDirectories(initCmd.fs, initCmd.home, initCmd.out); err != nil {
		return err
	}

	return ensureRepositoryFile(initCmd.fs, initCmd.home, initCmd.out)
}

func ensureRepositoryFile(fs afero.Fs, home kudohome.Home, out io.Writer) error {
	exists, err := afero.Exists(fs, home.RepositoryFile())
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(out, "Creating %s \n", home.RepositoryFile())
		r := repo.NewRepositories()
		if err := r.WriteFile(fs, home.RepositoryFile(), 0644); err != nil {
			return err
		}
	}

	return nil
}

func ensureDirectories(fs afero.Fs, home kudohome.Home, out io.Writer) error {
	dirs := []string{
		home.String(),
		home.Repository(),
	}
	for _, dir := range dirs {
		exists, err := afero.Exists(fs, dir)
		if err != nil {
			return err
		}
		if !exists {
			fmt.Fprintf(out, "Creating %s \n", dir)
			if err := fs.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("could not create %s: %s", dir, err)
			}
		}
	}
	return nil
}
