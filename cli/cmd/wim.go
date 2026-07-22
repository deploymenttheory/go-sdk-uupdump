package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/wim"
)

var wimCmd = &cobra.Command{
	Use:   "wim",
	Short: "Inspect and extract WIM/ESD images",
}

var wimInfoCmd = &cobra.Command{
	Use:   "info <file>",
	Short: "Print a WIM/ESD's header and image list",
	Args:  cobra.ExactArgs(1),
	RunE:  runWimInfo,
}

var wimTreeCmd = &cobra.Command{
	Use:   "tree <file>",
	Short: "List an image's directory tree",
	Args:  cobra.ExactArgs(1),
	RunE:  runWimTree,
}

var wimExtractCmd = &cobra.Command{
	Use:   "extract <file>",
	Short: "Extract an image's files to a directory",
	Args:  cobra.ExactArgs(1),
	RunE:  runWimExtract,
}

var wimSetPropertyCmd = &cobra.Command{
	Use:   "set-property <file> <key> <value>",
	Short: "Set an image XML property in place",
	Long: "Edit a WIM's image catalog in place (like wimlib_set_image_property).\n" +
		"Keys are top-level (NAME, DESCRIPTION, FLAGS, DISPLAYNAME) or a <WINDOWS>\n" +
		"child as WINDOWS/<TAG>. --image 0 (default) applies to every image.\n\n" +
		"Example — the Windows 11 bypass:\n" +
		"  winmediafoundry wim set-property install.wim WINDOWS/INSTALLATIONTYPE Server",
	Args: cobra.ExactArgs(3),
	RunE: runWimSetProperty,
}

func init() {
	wimTreeCmd.Flags().Int("image", 1, "1-based image index")
	wimTreeCmd.Flags().Int("depth", 3, "maximum path depth to print (0 = unlimited)")
	wimExtractCmd.Flags().Int("image", 1, "1-based image index")
	wimExtractCmd.Flags().StringP("out", "o", "", "destination directory (required)")
	_ = wimExtractCmd.MarkFlagRequired("out")
	wimSetPropertyCmd.Flags().Int("image", 0, "1-based image index (0 = all images)")

	wimCmd.AddCommand(wimInfoCmd, wimTreeCmd, wimExtractCmd, wimSetPropertyCmd)
	rootCmd.AddCommand(wimCmd)
}

func runWimInfo(_ *cobra.Command, args []string) error {
	w, err := wim.Open(args[0])
	if err != nil {
		return err
	}
	defer w.Close()

	info := w.Info()
	fmt.Printf("Compression: %s\n", info.Compression)
	fmt.Printf("Chunk size:  %d\n", info.ChunkSize)
	fmt.Printf("Images:      %d\n", info.ImageCount)
	fmt.Printf("Solid:       %t\n\n", info.Solid)

	t := newTable()
	fmt.Fprintln(t, "INDEX\tNAME\tEDITION\tARCH")
	for _, im := range w.Images() {
		fmt.Fprintf(t, "%d\t%s\t%s\t%s\n", im.Index, im.Name, im.Edition, im.Architecture)
	}
	return t.Flush()
}

func runWimTree(cmd *cobra.Command, args []string) error {
	w, err := wim.Open(args[0])
	if err != nil {
		return err
	}
	defer w.Close()

	index, _ := cmd.Flags().GetInt("image")
	depth, _ := cmd.Flags().GetInt("depth")
	root, err := w.OpenImage(index)
	if err != nil {
		return err
	}

	var files, dirs int
	root.Walk(func(path string, f *wim.File) {
		if f.IsDir() {
			dirs++
		} else {
			files++
		}
		if depth == 0 || strings.Count(path, "/")+1 <= depth {
			kind := "f"
			if f.IsDir() {
				kind = "d"
			}
			fmt.Printf("%s %d\t%s\n", kind, f.Size, path)
		}
	})
	fmt.Printf("\n%d directories, %d files\n", dirs, files)
	return nil
}

func runWimSetProperty(cmd *cobra.Command, args []string) error {
	path, key, value := args[0], args[1], args[2]
	u, err := wim.OpenForUpdate(path)
	if err != nil {
		return err
	}
	defer u.Close()

	image, _ := cmd.Flags().GetInt("image")
	if image == 0 {
		if err := u.SetPropertyAll(key, value); err != nil {
			return err
		}
	} else if err := u.SetProperty(image, key, value); err != nil {
		return err
	}
	if err := u.Commit(); err != nil {
		return err
	}

	scope := "all images"
	if image != 0 {
		scope = fmt.Sprintf("image %d", image)
	}
	fmt.Printf("set %s=%s on %s of %s\n", key, value, scope, path)
	return nil
}

func runWimExtract(cmd *cobra.Command, args []string) error {
	w, err := wim.Open(args[0])
	if err != nil {
		return err
	}
	defer w.Close()

	index, _ := cmd.Flags().GetInt("image")
	out, _ := cmd.Flags().GetString("out")
	fmt.Printf("Extracting image %d to %s ...\n", index, out)
	if err := w.ExtractImage(index, out); err != nil {
		return err
	}
	fmt.Println("done")
	return nil
}
