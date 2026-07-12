package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/betterleaks/betterleaks/sources"
)

var (
	fileScanMmapWindowBytes int64
	fileScanMmapSequential  bool
	fileScanMmapFD          int
)

func init() {
	rootCmd.AddCommand(fileScanMmapHelperCmd)
	fileScanMmapHelperCmd.Flags().Int64Var(
		&fileScanMmapWindowBytes,
		"window-bytes",
		32<<20,
		"mmap window size in bytes",
	)
	fileScanMmapHelperCmd.Flags().BoolVar(
		&fileScanMmapSequential,
		"sequential",
		false,
		"apply MADV_SEQUENTIAL to each window",
	)
	fileScanMmapHelperCmd.Flags().IntVar(
		&fileScanMmapFD,
		"fd",
		-1,
		"inherited file descriptor",
	)
}

var fileScanMmapHelperCmd = &cobra.Command{
	Use:    "_filescan-mmap-helper [flags] -- path",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if fileScanMmapFD >= 3 {
			file := os.NewFile(uintptr(fileScanMmapFD), args[0])
			if file == nil {
				return os.ErrInvalid
			}
			defer file.Close()
			return sources.RunFileScanMmapHelperFile(
				cmd.Context(),
				args[0],
				file,
				fileScanMmapWindowBytes,
				fileScanMmapSequential,
				os.Stdout,
			)
		}
		return sources.RunFileScanMmapHelper(
			cmd.Context(),
			args[0],
			fileScanMmapWindowBytes,
			fileScanMmapSequential,
			os.Stdout,
		)
	},
}
