package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Substack's official account, used as the example profile throughout.
const (
	notesTestHandle = "substack"
	notesTestUserID = 81309935
)

// fakeNotesSite serves the Substack endpoints the notes client uses: the
// public profile, the paginated notes feed, note images, and note videos.
type fakeNotesSite struct {
	*httptest.Server
	mu       sync.Mutex
	pages    []string // feed pages as JSON; page i links to page i+1
	requests map[string]int
}

func newFakeNotesSite(t *testing.T, pages ...[]map[string]any) *fakeNotesSite {
	t.Helper()
	s := &fakeNotesSite{requests: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	for i, items := range pages {
		next := ""
		if i < len(pages)-1 {
			next = fmt.Sprintf("cursor-%d", i+1)
		}
		page, err := json.Marshal(map[string]any{"items": items, "nextCursor": next})
		require.NoError(t, err)
		s.pages = append(s.pages, string(page))
	}
	return s
}

func (s *fakeNotesSite) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests[r.URL.Path]++
	s.mu.Unlock()
	switch {
	case r.URL.Path == "/api/v1/user/"+notesTestHandle+"/public_profile":
		fmt.Fprintf(w, `{"id":%d,"name":"Substack","handle":%q}`, notesTestUserID, notesTestHandle)
	case r.URL.Path == fmt.Sprintf("/api/v1/reader/feed/profile/%d", notesTestUserID):
		if r.URL.Query().Get("types[]") != "note" {
			http.Error(w, "missing types[]=note", http.StatusBadRequest)
			return
		}
		page := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			fmt.Sscanf(c, "cursor-%d", &page)
		}
		if page >= len(s.pages) {
			fmt.Fprint(w, `{"items":[],"nextCursor":null}`)
			return
		}
		fmt.Fprint(w, s.pages[page])
	case strings.HasPrefix(r.URL.Path, "/images/"):
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("fake image " + r.URL.Path))
	case strings.HasPrefix(r.URL.Path, "/api/v1/video/upload/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/video/upload/"), "/src")
		if strings.HasSuffix(r.URL.Path, "/src") {
			http.Redirect(w, r, "/cdn/"+id+".mp4", http.StatusTemporaryRedirect)
			return
		}
		fmt.Fprintf(w, `{"id":%q}`, id)
	case strings.HasPrefix(r.URL.Path, "/cdn/"):
		w.Write([]byte("fake video"))
	default:
		http.NotFound(w, r)
	}
}

func (s *fakeNotesSite) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[path]
}

// noteItem builds a feed item the way Substack returns one for a note.
func noteItem(id int64, userID int64, date, body string, attachments ...map[string]any) map[string]any {
	if attachments == nil {
		attachments = []map[string]any{}
	}
	return map[string]any{
		"entity_key": fmt.Sprintf("c-%d", id),
		"type":       "comment",
		"context":    map[string]any{"type": "note", "timestamp": date},
		"comment": map[string]any{
			"id": id, "type": "feed", "user_id": userID, "handle": notesTestHandle, "name": "Substack",
			"date": date, "edited_at": nil, "body": body, "attachments": attachments,
			"reaction_count": 3, "restacks": 1, "children_count": 0,
		},
	}
}

func imageAttachment(url string) map[string]any {
	return map[string]any{"id": "img-" + filepath.Base(url), "type": "image", "imageUrl": url, "imageWidth": 10, "imageHeight": 10}
}

func videoAttachment(uploadID string) map[string]any {
	return map[string]any{"id": "vid-" + uploadID, "type": "video", "media_upload_id": uploadID,
		"mediaUpload": map[string]any{"id": uploadID, "duration": 12.5}}
}

func newTestNotesClient(origin string) *NotesClient {
	return NewNotesClient(NewFetcher(WithRatePerSecond(1000)), origin)
}

func TestParseNotesProfile(t *testing.T) {
	tests := []struct {
		in, origin, handle string
		wantErr            bool
	}{
		{in: "substack", origin: "https://substack.com", handle: "substack"},
		{in: "@substack", origin: "https://substack.com", handle: "substack"},
		{in: "https://substack.com/@substack", origin: "https://substack.com", handle: "substack"},
		{in: "https://substack.com/@substack/notes", origin: "https://substack.com", handle: "substack"},
		{in: "http://127.0.0.1:8080/@substack", origin: "http://127.0.0.1:8080", handle: "substack"},
		{in: "", wantErr: true},
		{in: "https://substack.com/", wantErr: true},
		{in: "two words", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			origin, handle, err := ParseNotesProfile(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.origin, origin)
			assert.Equal(t, tt.handle, handle)
		})
	}
}

func TestNotesClientResolveUser(t *testing.T) {
	site := newFakeNotesSite(t)
	user, err := newTestNotesClient(site.URL).ResolveUser(context.Background(), notesTestHandle)
	require.NoError(t, err)
	assert.Equal(t, int64(notesTestUserID), user.ID)
	assert.Equal(t, notesTestHandle, user.Handle)

	_, err = newTestNotesClient(site.URL).ResolveUser(context.Background(), "nobody")
	assert.Error(t, err)
}

func TestNotesClientNotes(t *testing.T) {
	restack := noteItem(900, 12345, "2026-01-03T00:00:00.000Z", "someone else's note")
	restack["context"] = map[string]any{"type": "comment_restack"}
	site := newFakeNotesSite(t,
		[]map[string]any{
			noteItem(103, notesTestUserID, "2026-01-03T10:00:00.000Z", "third"),
			restack,
			noteItem(102, notesTestUserID, "2026-01-02T10:00:00.000Z", "second"),
		},
		[]map[string]any{
			noteItem(101, notesTestUserID, "2025-12-31T10:00:00.000Z", "first"),
		},
	)
	client := newTestNotesClient(site.URL)

	t.Run("follows the cursor and keeps only the user's own notes", func(t *testing.T) {
		notes, err := client.Notes(context.Background(), notesTestUserID, nil)
		require.NoError(t, err)
		var ids []int64
		for _, n := range notes {
			ids = append(ids, n.ID)
		}
		assert.Equal(t, []int64{103, 102, 101}, ids)
		assert.Equal(t, "third", notes[0].Body)
		assert.JSONEq(t, mustJSON(t, noteItem(103, notesTestUserID, "2026-01-03T10:00:00.000Z", "third")["comment"]), string(notes[0].Raw))
	})

	t.Run("applies the date filter to the note date", func(t *testing.T) {
		notes, err := client.Notes(context.Background(), notesTestUserID, func(date string) bool { return date > "2026-01-01" })
		require.NoError(t, err)
		require.Len(t, notes, 2)
		assert.Equal(t, int64(103), notes[0].ID)
		assert.Equal(t, int64(102), notes[1].ID)
	})
}

func TestNotesClientStopsOnRepeatedCursor(t *testing.T) {
	looping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := json.Marshal(map[string]any{
			"items":      []map[string]any{noteItem(1, notesTestUserID, "2026-01-01T00:00:00.000Z", "x")},
			"nextCursor": "same",
		})
		w.Write(page)
	}))
	t.Cleanup(looping.Close)
	notes, err := newTestNotesClient(looping.URL).Notes(context.Background(), notesTestUserID, nil)
	require.NoError(t, err)
	assert.Len(t, notes, 1)
}

func TestNoteBaseName(t *testing.T) {
	n := Note{ID: 42, Date: "2026-01-02T03:04:05.678Z"}
	assert.Equal(t, "20260102_030405_note-42", n.BaseName())
}

func TestNoteMarkdown(t *testing.T) {
	note := Note{
		ID: 7, Date: "2026-01-02T03:04:05.000Z", Handle: notesTestHandle, Name: "Substack",
		Body: "First paragraph.\n\nSecond paragraph.",
		Attachments: []NoteAttachment{
			{Type: "image", ImageURL: "https://cdn.example/images/a.jpeg"},
			{Type: "image", ImageURL: "https://cdn.example/images/b.jpeg"},
			{Type: "video", MediaUploadID: "v1"},
			{Type: "link", LinkMetadata: &NoteLink{URL: "https://example.com/x", Title: "Example"}},
			{Type: "post", Post: &NotePost{Title: "A post", CanonicalURL: "https://on.substack.com/p/a-post"}},
			{Type: "comment", Comment: &NoteQuote{ID: 9, Body: "Quoted line one\nline two", User: &NoteAuthor{Handle: "someone"}}},
		},
	}
	md := note.Markdown(
		map[string]string{"https://cdn.example/images/a.jpeg": "images/a.jpeg"},
		map[string]string{"v1": "videos/v1.mp4"},
	)
	assert.Contains(t, md, "# Note by Substack (@substack), 2026-01-02 03:04:05 UTC")
	assert.Contains(t, md, "https://substack.com/@substack/note/c-7")
	assert.Contains(t, md, "First paragraph.\n\nSecond paragraph.")
	assert.Contains(t, md, "![](images/a.jpeg)")
	assert.Contains(t, md, "![](https://cdn.example/images/b.jpeg)")
	assert.Contains(t, md, "[Video](videos/v1.mp4)")
	assert.Contains(t, md, "[Example](https://example.com/x)")
	assert.Contains(t, md, "[A post](https://on.substack.com/p/a-post)")
	assert.Contains(t, md, "> @someone: Quoted line one\n> line two")
}

func TestNotesClientDownloadImages(t *testing.T) {
	site := newFakeNotesSite(t)
	dir := t.TempDir()
	note := Note{ID: 5, Date: "2026-01-02T03:04:05.000Z", Attachments: []NoteAttachment{
		{Type: "image", ImageURL: site.URL + "/images/abc_1x1.jpeg"},
		{Type: "link", LinkMetadata: &NoteLink{URL: "https://example.com"}},
	}}
	client := newTestNotesClient(site.URL)

	paths, err := client.DownloadImages(context.Background(), note, dir, "images")
	require.NoError(t, err)
	require.Equal(t, map[string]string{site.URL + "/images/abc_1x1.jpeg": "images/abc_1x1.jpeg"}, paths)
	got, err := os.ReadFile(filepath.Join(dir, "images", "abc_1x1.jpeg"))
	require.NoError(t, err)
	assert.Equal(t, "fake image /images/abc_1x1.jpeg", string(got))

	_, err = client.DownloadImages(context.Background(), note, dir, "images")
	require.NoError(t, err)
	assert.Equal(t, 1, site.count("/images/abc_1x1.jpeg"), "existing images are not fetched again")
}

func TestNotesClientDownloadVideos(t *testing.T) {
	site := newFakeNotesSite(t)
	dir := t.TempDir()
	note := Note{ID: 6, Date: "2026-01-02T03:04:05.000Z", Attachments: []NoteAttachment{{Type: "video", MediaUploadID: "up1"}}}

	paths, err := newTestNotesClient(site.URL).DownloadVideos(context.Background(), note, dir, "videos")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"up1": "videos/20260102_030405_note-6_video-up1.mp4"}, paths)
	got, err := os.ReadFile(filepath.Join(dir, "videos", "20260102_030405_note-6_video-up1.mp4"))
	require.NoError(t, err)
	assert.Equal(t, "fake video", string(got))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
