package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// NotesClient downloads a user's Substack Notes, the short posts shown on a
// profile at substack.com/@handle, together with their images and videos.
type NotesClient struct {
	fetcher *Fetcher
	origin  string
}

// NewNotesClient creates a NotesClient that talks to origin, normally
// https://substack.com.
func NewNotesClient(fetcher *Fetcher, origin string) *NotesClient {
	return &NotesClient{fetcher: fetcher, origin: strings.TrimRight(origin, "/")}
}

// NoteAuthor is the user who wrote a note.
type NoteAuthor struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

// NoteLink is the preview of a link attached to a note.
type NoteLink struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// NotePost is a newsletter post attached to a note.
type NotePost struct {
	Title        string `json:"title"`
	CanonicalURL string `json:"canonical_url"`
}

// NoteQuote is another note quoted by a note.
type NoteQuote struct {
	ID   int64       `json:"id"`
	Body string      `json:"body"`
	User *NoteAuthor `json:"user"`
}

// NoteAttachment is an image, video, link, post, or quoted note attached to a
// note. Only the fields of its type are set.
type NoteAttachment struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	ImageURL      string     `json:"imageUrl,omitempty"`
	MediaUploadID string     `json:"media_upload_id,omitempty"`
	LinkMetadata  *NoteLink  `json:"linkMetadata,omitempty"`
	Post          *NotePost  `json:"post,omitempty"`
	Comment       *NoteQuote `json:"comment,omitempty"`
}

// Note is one note. Raw holds the note exactly as Substack returned it.
type Note struct {
	ID          int64            `json:"id"`
	UserID      int64            `json:"user_id"`
	Name        string           `json:"name"`
	Handle      string           `json:"handle"`
	Date        string           `json:"date"`
	EditedAt    *string          `json:"edited_at"`
	Body        string           `json:"body"`
	Attachments []NoteAttachment `json:"attachments"`
	Raw         json.RawMessage  `json:"-"`
}

var notesHandlePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ParseNotesProfile accepts a handle ("name" or "@name") or a profile URL
// ("https://substack.com/@name") and returns the origin to query and the
// handle.
func ParseNotesProfile(profile string) (origin, handle string, err error) {
	profile = strings.TrimSpace(profile)
	if !strings.Contains(profile, "://") {
		handle = strings.TrimPrefix(profile, "@")
		if !notesHandlePattern.MatchString(handle) {
			return "", "", fmt.Errorf("invalid Substack handle %q", profile)
		}
		return "https://substack.com", handle, nil
	}

	u, err := url.Parse(profile)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", fmt.Errorf("invalid Substack profile URL %q", profile)
	}
	first := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)[0]
	handle = strings.TrimPrefix(first, "@")
	if !strings.HasPrefix(first, "@") || !notesHandlePattern.MatchString(handle) {
		return "", "", fmt.Errorf("profile URL %q has no /@handle", profile)
	}
	return u.Scheme + "://" + u.Host, handle, nil
}

// ResolveUser looks up the user ID for a handle.
func (c *NotesClient) ResolveUser(ctx context.Context, handle string) (NoteAuthor, error) {
	var user NoteAuthor
	err := c.getJSON(ctx, c.origin+"/api/v1/user/"+url.PathEscape(handle)+"/public_profile", &user)
	if err != nil {
		return user, fmt.Errorf("failed to look up user %q: %w", handle, err)
	}
	if user.ID == 0 {
		return user, fmt.Errorf("user %q not found", handle)
	}
	return user, nil
}

// Notes returns the user's own notes, newest first, following the feed's
// cursor until it runs out. Restacks of other people's notes are skipped. If
// keep is not nil, only notes whose date it accepts are returned.
func (c *NotesClient) Notes(ctx context.Context, userID int64, keep DateFilterFunc) ([]Note, error) {
	var notes []Note
	seenNotes := map[int64]bool{}
	seenCursors := map[string]bool{}
	cursor := ""
	for {
		feedURL := fmt.Sprintf("%s/api/v1/reader/feed/profile/%d?types%%5B%%5D=note", c.origin, userID)
		if cursor != "" {
			feedURL += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items []struct {
				Context struct {
					Type string `json:"type"`
				} `json:"context"`
				Comment json.RawMessage `json:"comment"`
			} `json:"items"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.getJSON(ctx, feedURL, &page); err != nil {
			return nil, fmt.Errorf("failed to fetch notes: %w", err)
		}

		for _, item := range page.Items {
			if item.Context.Type != "note" || len(item.Comment) == 0 {
				continue
			}
			var note Note
			if err := json.Unmarshal(item.Comment, &note); err != nil {
				return nil, fmt.Errorf("failed to parse note: %w", err)
			}
			if note.UserID != userID || note.ID == 0 || seenNotes[note.ID] {
				continue
			}
			seenNotes[note.ID] = true
			if keep != nil && !keep(note.Date) {
				continue
			}
			note.Raw = append(json.RawMessage(nil), item.Comment...)
			notes = append(notes, note)
		}

		if page.NextCursor == "" || len(page.Items) == 0 || seenCursors[page.NextCursor] {
			return notes, nil
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

func (c *NotesClient) getJSON(ctx context.Context, url string, v any) error {
	body, err := c.fetcher.FetchURL(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(body).Decode(v)
}

// noteTime parses the note date, which Substack gives in RFC 3339.
func (n Note) noteTime() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, n.Date)
	return t.UTC(), err == nil
}

// BaseName is the file name, without extension, for the note's files:
// <YYYYMMDD_HHMMSS>_note-<ID>, so notes sort by date like posts do.
func (n Note) BaseName() string {
	stamp := "00000000_000000"
	if t, ok := n.noteTime(); ok {
		stamp = t.Format("20060102_150405")
	}
	return fmt.Sprintf("%s_note-%d", stamp, n.ID)
}

// URL is the note's public address.
func (n Note) URL() string {
	return fmt.Sprintf("https://substack.com/@%s/note/c-%d", n.Handle, n.ID)
}

// Markdown renders the note. images maps image URLs, and videos maps upload
// IDs, to local paths; attachments without a local copy link to Substack.
func (n Note) Markdown(images, videos map[string]string) string {
	var b strings.Builder
	when := n.Date
	if t, ok := n.noteTime(); ok {
		when = t.Format("2006-01-02 15:04:05 UTC")
	}
	fmt.Fprintf(&b, "# Note by %s (@%s), %s\n\n", n.Name, n.Handle, when)
	fmt.Fprintf(&b, "%s\n\n", n.URL())
	if n.EditedAt != nil && *n.EditedAt != "" {
		fmt.Fprintf(&b, "Edited %s\n\n", *n.EditedAt)
	}
	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString(body + "\n\n")
	}

	for _, a := range n.Attachments {
		switch a.Type {
		case "image":
			src := a.ImageURL
			if local, ok := images[a.ImageURL]; ok {
				src = local
			}
			if src != "" {
				fmt.Fprintf(&b, "![](%s)\n\n", src)
			}
		case "video":
			if local, ok := videos[a.MediaUploadID]; ok {
				fmt.Fprintf(&b, "[Video](%s)\n\n", local)
			} else {
				fmt.Fprintf(&b, "[Video](%s)\n\n", n.URL())
			}
		case "link":
			if a.LinkMetadata != nil && a.LinkMetadata.URL != "" {
				fmt.Fprintf(&b, "[%s](%s)\n\n", linkTitle(a.LinkMetadata.Title, a.LinkMetadata.URL), a.LinkMetadata.URL)
			}
		case "post":
			if a.Post != nil && a.Post.CanonicalURL != "" {
				fmt.Fprintf(&b, "[%s](%s)\n\n", linkTitle(a.Post.Title, a.Post.CanonicalURL), a.Post.CanonicalURL)
			}
		case "comment":
			if a.Comment != nil {
				who := "note"
				if a.Comment.User != nil && a.Comment.User.Handle != "" {
					who = "@" + a.Comment.User.Handle
				}
				lines := strings.Split(strings.TrimSpace(a.Comment.Body), "\n")
				lines[0] = who + ": " + lines[0]
				for _, line := range lines {
					b.WriteString(strings.TrimRight("> "+line, " ") + "\n")
				}
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func linkTitle(title, fallback string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return fallback
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// DownloadImages saves the note's images to <dir>/<imagesDir>/ under the name
// in their URL and returns their paths relative to dir, keyed by URL. Images
// already on disk are not fetched again.
func (c *NotesClient) DownloadImages(ctx context.Context, n Note, dir, imagesDir string) (map[string]string, error) {
	paths := map[string]string{}
	var errs []error
	for _, a := range n.Attachments {
		if a.Type != "image" || a.ImageURL == "" {
			continue
		}
		u, err := url.Parse(a.ImageURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("invalid image URL %q", a.ImageURL))
			continue
		}
		name := unsafeFileChars.ReplaceAllString(path.Base(u.Path), "_")
		if name == "" || name == "." || name == "_" {
			name = fmt.Sprintf("note-%d-image-%s", n.ID, unsafeFileChars.ReplaceAllString(a.ID, "_"))
		}
		rel := filepath.ToSlash(filepath.Join(imagesDir, name))
		dest := filepath.Join(dir, imagesDir, name)

		if stat, err := os.Lstat(dest); err == nil && stat.Mode().IsRegular() && stat.Size() > 0 {
			paths[a.ImageURL] = rel
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return paths, fmt.Errorf("failed to create images directory: %w", err)
		}
		body, err := c.fetcher.FetchURL(ctx, a.ImageURL)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to download %s: %w", a.ImageURL, err))
			continue
		}
		err = writeFileAtomicallyWith(dest, func(f *os.File) error {
			_, err := io.Copy(f, body)
			return err
		})
		body.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to save %s: %w", a.ImageURL, err))
			continue
		}
		paths[a.ImageURL] = rel
	}
	return paths, errors.Join(errs...)
}

// DownloadVideos saves the note's videos to <dir>/<videosDir>/ as
// <BaseName>_video-<ID>.mp4 and returns their paths relative to dir, keyed by
// upload ID. Videos already on disk are not fetched again.
func (c *NotesClient) DownloadVideos(ctx context.Context, n Note, dir, videosDir string) (map[string]string, error) {
	paths := map[string]string{}
	vd := NewVideoDownloader(c.fetcher, dir, videosDir)
	var errs []error
	for _, a := range n.Attachments {
		if a.Type != "video" || !uploadIDPattern.MatchString(a.MediaUploadID) {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dir, videosDir), 0755); err != nil {
			return paths, fmt.Errorf("failed to create videos directory: %w", err)
		}
		base := filepath.Join(dir, videosDir, n.BaseName()+"_video-"+a.MediaUploadID)
		info := vd.downloadVideo(ctx, c.origin+"/api/v1/video/upload/"+a.MediaUploadID, base)
		if !info.Success {
			errs = append(errs, fmt.Errorf("video %s: %w", a.MediaUploadID, info.Error))
			continue
		}
		if rel, err := filepath.Rel(dir, info.LocalPath); err == nil {
			paths[a.MediaUploadID] = filepath.ToSlash(rel)
		}
	}
	return paths, errors.Join(errs...)
}
