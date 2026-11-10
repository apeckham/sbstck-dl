package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNotesProfile serves Substack's official account (@substack) with a
// small notes feed: one page of notes, a restack of someone else's note, and
// a second page reached through the cursor.
type fakeNotesProfile struct {
	*httptest.Server
	mu       sync.Mutex
	requests map[string]int
}

const notesCLIUserID = 81309935

func newFakeNotesProfile(t *testing.T) *fakeNotesProfile {
	p := &fakeNotesProfile{requests: map[string]int{}}
	// Started after the assignment so handlers never race on p.Server.
	p.Server = httptest.NewUnstartedServer(http.HandlerFunc(p.serve))
	p.Start()
	t.Cleanup(p.Close)
	return p
}

func (p *fakeNotesProfile) note(id int64, userID int64, date, body string, attachments ...map[string]any) map[string]any {
	if attachments == nil {
		attachments = []map[string]any{}
	}
	return map[string]any{
		"type":    "comment",
		"context": map[string]any{"type": "note"},
		"comment": map[string]any{"id": id, "user_id": userID, "name": "Substack", "handle": "substack",
			"date": date, "edited_at": nil, "body": body, "attachments": attachments},
	}
}

func (p *fakeNotesProfile) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests[r.URL.Path]++
	p.mu.Unlock()
	feed := fmt.Sprintf("/api/v1/reader/feed/profile/%d", notesCLIUserID)
	switch {
	case r.URL.Path == "/api/v1/user/substack/public_profile":
		fmt.Fprintf(w, `{"id":%d,"name":"Substack","handle":"substack"}`, notesCLIUserID)
	case r.URL.Path == feed && r.URL.Query().Get("cursor") == "":
		restack := p.note(900, 4242, "2026-01-03T09:00:00.000Z", "someone else")
		restack["context"] = map[string]any{"type": "comment_restack"}
		json.NewEncoder(w).Encode(map[string]any{"nextCursor": "page-2", "items": []any{
			p.note(103, notesCLIUserID, "2026-01-03T10:00:00.000Z", "Newest note.",
				map[string]any{"id": "i1", "type": "image", "imageUrl": p.URL + "/images/a_1x1.jpeg"}),
			restack,
			p.note(102, notesCLIUserID, "2026-01-02T10:00:00.000Z", "A note with a video.",
				map[string]any{"id": "v1", "type": "video", "media_upload_id": "up9"}),
		}})
	case r.URL.Path == feed && r.URL.Query().Get("cursor") == "page-2":
		json.NewEncoder(w).Encode(map[string]any{"nextCursor": nil, "items": []any{
			p.note(101, notesCLIUserID, "2025-12-31T10:00:00.000Z", "Oldest note."),
		}})
	case strings.HasPrefix(r.URL.Path, "/images/"):
		w.Write([]byte("fake image"))
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

func (p *fakeNotesProfile) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[path]
}

func TestNotesCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "sbstck-dl")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	output, err := exec.Command("go", "build", "-o", binary, "..").CombinedOutput()
	require.NoError(t, err, "%s", output)
	run := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		timeout, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(timeout, binary, append([]string{"notes", "--rate", "1000"}, args...)...).CombinedOutput()
		require.NoError(t, timeout.Err(), "CLI stalled: %s", out)
		return string(out), err
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(b)
	}

	t.Run("saves the user's notes with their images and videos", func(t *testing.T) {
		site := newFakeNotesProfile(t)
		dir := t.TempDir()
		out, err := run(t, "--user", site.URL+"/@substack", "--output", dir, "--format", "md,json", "--download-images", "--download-videos")
		require.NoError(t, err, out)

		for _, name := range []string{"20260103_100000_note-103", "20260102_100000_note-102", "20251231_100000_note-101"} {
			assert.FileExists(t, filepath.Join(dir, name+".md"))
			assert.FileExists(t, filepath.Join(dir, name+".json"))
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "*note-900*"))
		assert.Empty(t, matches, "restacks of other people's notes are skipped")

		newest := read(t, filepath.Join(dir, "20260103_100000_note-103.md"))
		assert.Contains(t, newest, "Newest note.")
		assert.Contains(t, newest, "![](images/a_1x1.jpeg)")
		assert.Equal(t, "fake image", read(t, filepath.Join(dir, "images", "a_1x1.jpeg")))

		withVideo := read(t, filepath.Join(dir, "20260102_100000_note-102.md"))
		assert.Contains(t, withVideo, "[Video](videos/20260102_100000_note-102_video-up9.mp4)")
		assert.Equal(t, "fake video", read(t, filepath.Join(dir, "videos", "20260102_100000_note-102_video-up9.mp4")))

		var raw map[string]any
		require.NoError(t, json.Unmarshal([]byte(read(t, filepath.Join(dir, "20260103_100000_note-103.json"))), &raw))
		assert.Equal(t, float64(103), raw["id"])
		assert.Equal(t, "Newest note.", raw["body"])
		assert.Contains(t, out, "3 notes")

		_, err = run(t, "--user", site.URL+"/@substack", "--output", dir, "--format", "md,json", "--download-images", "--download-videos")
		require.NoError(t, err)
		assert.Equal(t, 1, site.count("/images/a_1x1.jpeg"), "a re-run does not fetch images again")
		assert.Equal(t, 1, site.count("/cdn/up9.mp4"), "a re-run does not fetch videos again")
	})

	t.Run("writes markdown only by default and skips media unless asked", func(t *testing.T) {
		site := newFakeNotesProfile(t)
		dir := t.TempDir()
		out, err := run(t, "--user", site.URL+"/@substack", "--output", dir)
		require.NoError(t, err, out)
		assert.FileExists(t, filepath.Join(dir, "20260103_100000_note-103.md"))
		assert.NoFileExists(t, filepath.Join(dir, "20260103_100000_note-103.json"))
		assert.NoDirExists(t, filepath.Join(dir, "images"))
		assert.Contains(t, read(t, filepath.Join(dir, "20260103_100000_note-103.md")), "![]("+site.URL+"/images/a_1x1.jpeg)")
	})

	t.Run("filters notes by date", func(t *testing.T) {
		site := newFakeNotesProfile(t)
		dir := t.TempDir()
		out, err := run(t, "--user", site.URL+"/@substack", "--output", dir, "--after", "2026-01-03")
		require.NoError(t, err, out)
		files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		require.Len(t, files, 1)
		assert.Equal(t, "20260103_100000_note-103.md", filepath.Base(files[0]))
	})

	t.Run("rejects an invalid profile", func(t *testing.T) {
		out, err := run(t, "--user", "https://substack.com/no-handle", "--output", t.TempDir())
		assert.Error(t, err)
		assert.Contains(t, out, "no /@handle")
	})
}
