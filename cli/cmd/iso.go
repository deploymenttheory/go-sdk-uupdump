package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/builder"
)

var isoCmd = &cobra.Command{
	Use:   "iso",
	Short: "Build bootable ISOs",
}

var isoBuildCmd = &cobra.Command{
	Use:   "build <esd> <out.iso>",
	Short: "Build a bootable Windows ISO from an ESD",
	Long: "Extract the Setup Media skeleton, rebuild sources/boot.wim and " +
		"sources/install.wim, and master a UDF + El Torito ISO.",
	Args: cobra.ExactArgs(2),
	RunE: runISOBuild,
}

func init() {
	isoBuildCmd.Flags().StringP("label", "l", "CCCOMA_X64FRE", "ISO volume label")
	isoBuildCmd.Flags().String("workdir", "", "scratch directory for the media tree (kept if set; temp otherwise)")
	isoBuildCmd.Flags().Bool("bypass-win11", false, "apply all Windows 11 requirement bypasses (INSTALLATIONTYPE=Server + LabConfig + BypassNRO)")
	isoBuildCmd.Flags().Bool("bypass-installtype-server", false, "set install.wim INSTALLATIONTYPE=Server (skips the whole Win11 hardware appraisal)")
	isoBuildCmd.Flags().Bool("bypass-labconfig", false, "add an autounattend.xml with LabConfig TPM/SecureBoot/RAM/Storage/CPU bypass keys")
	isoBuildCmd.Flags().Bool("bypass-nro", false, "add OOBE\\BypassNRO to the generated autounattend.xml (skip the online-account requirement)")
	isoCmd.AddCommand(isoBuildCmd)
	rootCmd.AddCommand(isoCmd)
}

func runISOBuild(cmd *cobra.Command, args []string) error {
	label, _ := cmd.Flags().GetString("label")
	workdir, _ := cmd.Flags().GetString("workdir")
	bypass := bypassFromFlags(cmd)
	fmt.Printf("Building %s from %s ...\n", args[1], args[0])

	start := time.Now()
	if err := builder.BuildISO(args[0], args[1], builder.Options{
		VolumeID: label,
		WorkDir:  workdir,
		Bypass:   bypass,
		Progress: cmd.OutOrStdout(),
	}); err != nil {
		return err
	}
	fmt.Printf("done in %s\n", time.Since(start).Round(time.Second))
	return nil
}

// bypassFromFlags assembles a builder.Win11Bypass from the --bypass-* flags.
// --bypass-win11 turns on every mechanism; the granular flags enable them
// individually and compose with it.
func bypassFromFlags(cmd *cobra.Command) builder.Win11Bypass {
	all, _ := cmd.Flags().GetBool("bypass-win11")
	server, _ := cmd.Flags().GetBool("bypass-installtype-server")
	lab, _ := cmd.Flags().GetBool("bypass-labconfig")
	nro, _ := cmd.Flags().GetBool("bypass-nro")
	return builder.Win11Bypass{
		InstallationTypeServer: all || server,
		LabConfig:              all || lab,
		BypassNRO:              all || nro,
	}
}
