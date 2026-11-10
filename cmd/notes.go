package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexferrari88/sbstck-dl/lib"
	"github.com/spf13/cobra"
)

var (
	notesProfile        string
	notesOutput         string
	notesFormat         string
	notesDownloadImages bool
	notesDownloadVideos bool
	notesImagesDir      string
	notesVideosDir      string

	notesCmd = &cobra.Command{
		Use:   "notes",
		Short: "Download a user's Substack Notes",
		Long: `Download the Substack Notes a user has posted, the short posts on their profile at substack.com/@handle.

Each note is saved as <YYYYMMDD_HHMMSS>_note-<ID> in the chosen formats: md (readable text with attachments) and json (the note exactly as Substack returns it). Restacks of other people's notes are skipped. Images and Substack-hosted videos attached to notes are downloaded with --download-images and --download-videos; files already on disk are kept.`,
		Example: `  sbstck-dl notes --user substack --output notes
  sbstck-dl notes --user https://substack.com/@substack --output notes --format md,json --download-images --download-videos`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runNotes(cmd)
		},
	}
)

func init() {
	notesCmd.Flags().StringVarP(&notesProfile, "user", "u", "", "Substack handle (substack or @substack) or profile URL (https://substack.com/@substack)")
	notesCmd.Flags().StringVarP(&notesOutput, "output", "o", ".", "Directory to save notes in")
	notesCmd.Flags().StringVarP(&notesFormat, "format", "f", "md", "Comma-separated note formats: md, json")
	notesCmd.Flags().BoolVar(&notesDownloadImages, "download-images", false, "Download images attached to notes")
	notesCmd.Flags().BoolVar(&notesDownloadVideos, "download-videos", false, "Download Substack-hosted videos attached to notes")
	notesCmd.Flags().StringVar(&notesImagesDir, "images-dir", "images", "Directory name for downloaded note images")
	notesCmd.Flags().StringVar(&notesVideosDir, "videos-dir", "videos", "Directory name for downloaded note videos")
	notesCmd.MarkFlagRequired("user")
}

func parseNotesFormats(value string) ([]string, error) {
	var formats []string
	seen := map[string]bool{}
	for _, f := range strings.Split(value, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" || seen[f] {
			continue
		}
		if f != "md" && f != "json" {
			return nil, fmt.Errorf("unsupported notes format %q (use md or json)", f)
		}
		seen[f] = true
		formats = append(formats, f)
	}
	if len(formats) == 0 {
		return nil, errors.New("no notes format given")
	}
	return formats, nil
}

func runNotes(cmd *cobra.Command) error {
	formats, err := parseNotesFormats(notesFormat)
	if err != nil {
		return err
	}
	origin, handle, err := lib.ParseNotesProfile(notesProfile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(notesOutput, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	client := lib.NewNotesClient(fetcher, origin)
	user, err := client.ResolveUser(ctx, handle)
	if err != nil {
		return err
	}
	if verbose {
		fmt.Printf("Fetching notes by @%s (user %d)...\n", user.Handle, user.ID)
	}
	notes, err := client.Notes(ctx, user.ID, makeDateFilterFunc(beforeDate, afterDate))
	if err != nil {
		return err
	}

	var images, videos int
	var mediaErrs []error
	for _, note := range notes {
		imagePaths := map[string]string{}
		if notesDownloadImages {
			paths, err := client.DownloadImages(ctx, note, notesOutput, notesImagesDir)
			if err != nil {
				mediaErrs = append(mediaErrs, fmt.Errorf("note %d: %w", note.ID, err))
			}
			imagePaths = paths
			images += len(paths)
		}
		videoPaths := map[string]string{}
		if notesDownloadVideos {
			paths, err := client.DownloadVideos(ctx, note, notesOutput, notesVideosDir)
			if err != nil {
				mediaErrs = append(mediaErrs, fmt.Errorf("note %d: %w", note.ID, err))
			}
			videoPaths = paths
			videos += len(paths)
		}

		base := filepath.Join(notesOutput, note.BaseName())
		for _, f := range formats {
			var data []byte
			switch f {
			case "md":
				data = []byte(note.Markdown(imagePaths, videoPaths))
			case "json":
				data = append(append([]byte(nil), note.Raw...), '\n')
			}
			if err := os.WriteFile(base+"."+f, data, 0644); err != nil {
				return fmt.Errorf("failed to save note %d: %w", note.ID, err)
			}
		}
	}

	summary := fmt.Sprintf("Saved %d notes by @%s to %s", len(notes), user.Handle, notesOutput)
	if notesDownloadImages {
		summary += fmt.Sprintf(", %d images", images)
	}
	if notesDownloadVideos {
		summary += fmt.Sprintf(", %d videos", videos)
	}
	fmt.Fprintln(cmd.OutOrStdout(), summary)

	if len(mediaErrs) > 0 {
		for _, e := range mediaErrs {
			fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", e)
		}
		return fmt.Errorf("%d notes had media that failed to download", len(mediaErrs))
	}
	return nil
}
