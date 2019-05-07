package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	kube "github.com/ian-howell/airshipctl/pkg/kube"
)

const versionLong = `Show the versions for the airshipctl tool and its components.
This includes the following components, in order:
  * airshipctl client
  * kubernetes cluster
`

// NewVersionCommand prints out the versions of airshipctl and its underlying tools
func NewVersionCommand(out io.Writer, client *kube.Client) *cobra.Command {
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Show the version number of airshipctl and its underlying tools",
		Long:  versionLong,
		Run: func(cmd *cobra.Command, args []string) {
			clientV := clientVersion()
			kubeV, err := kubeVersion(client)
			if err != nil {
				fmt.Fprintf(out, "Could not get kubernetes version")
				return
			}

			w := tabwriter.NewWriter(out, 0, 0, 1, ' ', 0)
			defer w.Flush()
			fmt.Fprintf(w, "%s:\t%s\n", "client", clientV)
			fmt.Fprintf(w, "%s:\t%s\n", "kubernetes server", kubeV)
		},
	}
	return versionCmd
}

func clientVersion() string {
	// TODO(howell): There's gotta be a smarter way to do this
	return "v0.1.0"
}

func kubeVersion(client *kube.Client) (string, error) {
	if client == nil {
		return "could not connect to a kubernetes cluster", nil
	}
	version, err := client.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return version.String(), nil
}
