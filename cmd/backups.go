package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/boxctl-io/boxctl-cli/internal/client"
	"github.com/spf13/cobra"
)

var backupsCmd = &cobra.Command{
	Use:     "backups",
	Aliases: []string{"backup"},
	Short:   "List your backups",
	Long: `List your backups, newest first -- the saved copies of paused boxes
that 'boxctl download' and the dashboard's Backup button create.

Bring one back as a new box with 'boxctl restore'.
Delete one with 'boxctl backups rm'.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, err := wantJSON(cmd)
		if err != nil {
			return err
		}
		backups, err := listBackups(cmd.Context())
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(backups)
		}
		if len(backups) == 0 {
			fmt.Println("No backups yet. Pause a box and run `boxctl download <name>` to make one.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "ID\tBOX\tSIZE\tSTATUS\tAGE"); err != nil {
			return err
		}
		for _, b := range backups {
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", b.ID, b.VMName, client.FormatBytes(b.SizeBytes), b.UploadStatus, age(b.CreatedAt)); err != nil {
				return err
			}
		}
		return w.Flush()
	},
}

// listBackups is the caller's backups, newest first.
func listBackups(ctx context.Context) ([]client.Backup, error) {
	backups, err := newClient().ListBackups(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(backups, func(i, j int) bool { return backups[i].CreatedAt.After(backups[j].CreatedAt) })
	return backups, nil
}

func init() {
	addOutputFlag(backupsCmd)
	rootCmd.AddCommand(backupsCmd)
}

var backupsRmForce bool

var backupsRmCmd = &cobra.Command{
	Use:     "rm <backup>...",
	Aliases: []string{"delete"},
	Short:   "Delete backups",
	Long: `Delete one or more backups for good. Each <backup> is a backup's id (see
'boxctl backups'), or the name of the box it was taken from, meaning that
box's newest backup.`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: completeBackup,
	RunE: func(cmd *cobra.Command, args []string) error {
		backups, err := listBackups(cmd.Context())
		if err != nil {
			return err
		}
		var picked []client.Backup
		seen := map[string]bool{}
		for _, a := range args {
			b, err := findBackup(backups, a)
			if err != nil {
				return err
			}
			if !seen[b.ID] {
				seen[b.ID] = true
				picked = append(picked, b)
			}
		}
		if !backupsRmForce {
			var ids []string
			for _, b := range picked {
				ids = append(ids, b.ID)
			}
			if !confirm(fmt.Sprintf("Delete backup %s? This cannot be undone.", strings.Join(ids, ", "))) {
				return nil
			}
		}
		c := newClient()
		failed := 0
		for _, b := range picked {
			if err := c.DeleteBackup(cmd.Context(), b.ID); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "✘ %s: %v\n", b.ID, err)
				failed++
				continue
			}
			fmt.Printf("✔ Deleted backup %s (%s)\n", b.ID, b.VMName)
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d backups could not be deleted", failed, len(picked))
		}
		return nil
	},
}

func init() {
	backupsRmCmd.Flags().BoolVarP(&backupsRmForce, "force", "f", false, "skip the confirmation prompt")
	backupsCmd.AddCommand(backupsRmCmd)
}
