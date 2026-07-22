package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/usb"
)

var usbCmd = &cobra.Command{
	Use:   "usb",
	Short: "Write bootable Windows USB drives",
}

var usbListCmd = &cobra.Command{
	Use:   "list",
	Short: "List candidate target disks",
	Args:  cobra.NoArgs,
	RunE:  runUSBList,
}

var usbWriteCmd = &cobra.Command{
	Use:   "write <mediaDir>",
	Short: "Partition, format, and write a Windows media tree to a disk",
	Long: "Write an extracted Windows media tree (with sources/boot.wim and\n" +
		"sources/install.wim) to a USB disk. install.wim over the FAT32 4 GiB\n" +
		"limit is split into an install.swm set; a missing EFI loader is\n" +
		"synthesized from install.wim.\n\n" +
		"This ERASES the target disk. Use --dry-run to preview, then --yes to run.",
	Args: cobra.ExactArgs(1),
	RunE: runUSBWrite,
}

func init() {
	usbWriteCmd.Flags().String("disk", "", "target disk id (from 'usb list'); required unless --dry-run")
	usbWriteCmd.Flags().String("scheme", "mbr", "partition scheme: mbr or gpt")
	usbWriteCmd.Flags().String("fs", "fat32", "file system: fat32 or exfat")
	usbWriteCmd.Flags().StringP("label", "l", "WINMEDIA", "volume label")
	usbWriteCmd.Flags().Bool("yes", false, "confirm the destructive write (required to actually write)")
	usbWriteCmd.Flags().Bool("force", false, "allow writing to a non-removable (fixed) disk")
	usbWriteCmd.Flags().Bool("dry-run", false, "print the write plan without touching any disk")

	usbCmd.AddCommand(usbListCmd, usbWriteCmd)
	rootCmd.AddCommand(usbCmd)
}

func runUSBList(_ *cobra.Command, _ []string) error {
	disks, err := usb.ListDisks()
	if err != nil {
		return err
	}
	if len(disks) == 0 {
		fmt.Println("no disks found")
		return nil
	}
	t := newTable()
	fmt.Fprintln(t, "ID\tMODEL\tSIZE\tREMOVABLE")
	for _, d := range disks {
		fmt.Fprintf(t, "%s\t%s\t%.1f GB\t%t\n", d.ID, d.Model, float64(d.SizeBytes)/1e9, d.Removable)
	}
	return t.Flush()
}

func runUSBWrite(cmd *cobra.Command, args []string) error {
	mediaDir := args[0]
	opts := usb.Options{
		Scheme:      usb.Scheme(mustString(cmd, "scheme")),
		FS:          usb.FS(mustString(cmd, "fs")),
		VolumeLabel: mustString(cmd, "label"),
		Force:       mustBool(cmd, "force"),
	}

	plan, err := usb.DryRun(mediaDir, opts)
	if err != nil {
		return err
	}
	fmt.Printf("Plan for %s:\n", mediaDir)
	fmt.Printf("  scheme=%s fs=%s label=%s\n", plan.Scheme, plan.FS, opts.VolumeLabel)
	fmt.Printf("  files=%d (%.2f GB)\n", plan.FileCount, float64(plan.TotalBytes)/1e9)
	fmt.Printf("  split install.wim -> install.swm: %t\n", plan.SplitInstallWIM)
	fmt.Printf("  synthesize efi/boot/boot%s.efi: %t\n", plan.EFIArch, plan.EFIFallback)

	if mustBool(cmd, "dry-run") {
		return nil
	}
	diskID := mustString(cmd, "disk")
	if diskID == "" {
		return fmt.Errorf("--disk is required (see 'usb list'); or use --dry-run")
	}
	if !mustBool(cmd, "yes") {
		return fmt.Errorf("refusing to erase disk %s without --yes", diskID)
	}

	// Resolve the real disk so its actual removable flag is enforced by
	// WriteMedia (a fixed disk still requires --force).
	target, err := resolveDisk(diskID)
	if err != nil {
		return err
	}
	opts.Progress = cmd.OutOrStdout()
	fmt.Printf("Writing to disk %s (%s) — this erases it...\n", target.ID, target.Model)
	if err := usb.WriteMedia(context.Background(), target, mediaDir, opts); err != nil {
		return err
	}
	fmt.Println("done")
	return nil
}

// resolveDisk finds the enumerated disk with the given id so its true Removable
// flag governs the safety check.
func resolveDisk(id string) (usb.Disk, error) {
	disks, err := usb.ListDisks()
	if err != nil {
		return usb.Disk{}, err
	}
	for _, d := range disks {
		if d.ID == id {
			return d, nil
		}
	}
	return usb.Disk{}, fmt.Errorf("disk %q not found (see 'usb list')", id)
}

func mustBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}
