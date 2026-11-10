package cmd

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/alexferrari88/sbstck-dl/lib"
	"github.com/stretchr/testify/require"
)

func TestGalleryCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "sbstck-dl")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	output, err := exec.Command("go", "build", "-o", binary, "..").CombinedOutput()
	require.NoError(t, err, string(output))
	var mu sync.Mutex
	counts := map[string]int{}
	var server *httptest.Server
	// Created unstarted: the handler reads server.URL, which must be set first.
	server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		switch {
		case r.URL.Path == "/p/trip":
			attrs := fmt.Sprintf(`{"gallery":{"images":[{"type":"image/jpeg","src":"%[1]s/img/a.jpeg"},{"type":"image/jpeg","src":"%[1]s/img/b.jpeg"}],"caption":"Our trip","alt":"","staticGalleryImage":{"type":"image/png","src":"%[1]s/img/grid.png"}},"isEditorNode":true}`, server.URL)
			body := `<p>Before</p><div class="image-gallery-embed" data-attrs="` + html.EscapeString(attrs) + `"></div><p>After</p>`
			post := lib.Post{Slug: "trip", Title: "Trip", PostDate: "2026-01-01T12:00:00Z", CanonicalUrl: server.URL + r.URL.Path, BodyHTML: body}
			payload, _ := json.Marshal(lib.PostWrapper{Post: post})
			escaped, _ := json.Marshal(string(payload))
			fmt.Fprintf(w, "<script>window._preloads = JSON.parse(%s)</script>", escaped)
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/jpeg")
			fmt.Fprint(w, "image bytes "+r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	server.Start()
	defer server.Close()
	count := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[path]
	}
	read := func(t *testing.T, dir, name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	run := func(t *testing.T, dir string, flags ...string) {
		t.Helper()
		args := append([]string{"download", "--url", server.URL + "/p/trip", "--output", dir, "--rate", "1000"}, flags...)
		out, err := exec.Command(binary, args...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	const stem = "20260101_120000_trip"

	t.Run("downloads gallery images and shows them locally", func(t *testing.T) {
		dir := t.TempDir()
		run(t, dir, "-f", "html,md,txt", "--download-images")
		for _, name := range []string{"a.jpeg", "b.jpeg", "grid.png"} {
			require.Equal(t, "image bytes /img/"+name, read(t, dir, filepath.Join("images", "trip", name)))
			require.Equal(t, 1, count("/img/"+name))
		}
		post := read(t, dir, stem+".html")
		require.Contains(t, post, `<img src="images/trip/a.jpeg"`)
		require.Contains(t, post, `<figcaption>Our trip</figcaption>`)
		require.Less(t, strings.Index(post, "Before"), strings.Index(post, "images/trip/a.jpeg"))
		require.Less(t, strings.Index(post, "images/trip/b.jpeg"), strings.Index(post, "After"))
		require.Contains(t, read(t, dir, stem+".md"), "](images/trip/b.jpeg)")
		require.Contains(t, read(t, dir, stem+".txt"), "Our trip")
	})

	t.Run("without downloading, galleries show their remote images", func(t *testing.T) {
		dir := t.TempDir()
		run(t, dir, "-f", "html")
		require.Contains(t, read(t, dir, stem+".html"), `<img src="`+server.URL+`/img/a.jpeg"`)
		_, err := os.Stat(filepath.Join(dir, "images"))
		require.True(t, os.IsNotExist(err))
	})
}
