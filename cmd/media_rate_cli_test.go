package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexferrari88/sbstck-dl/lib"
	"github.com/stretchr/testify/require"
)

// The limiters allow a burst of lib.DefaultBurst requests, then --rate or
// --media-rate requests per second. With rates of 1, requests that arrive
// within half a second of each other came from the burst.
func TestMediaRateCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "sbstck-dl")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	output, err := exec.Command("go", "build", "-o", binary, "..").CombinedOutput()
	require.NoError(t, err, "%s", output)
	const images = 6
	type request struct {
		path string
		at   time.Time
	}
	var mu sync.Mutex
	var requests []request
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, request{r.URL.Path, time.Now()})
		mu.Unlock()
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/p/"):
			slug := strings.TrimPrefix(path, "/p/")
			post := map[string]any{"slug": slug, "title": slug, "post_date": "2025-01-02T03:04:05Z", "canonical_url": server.URL + path, "audience": "everyone"}
			var body strings.Builder
			switch slug {
			case "gallery":
				for i := 0; i < images; i++ {
					fmt.Fprintf(&body, `<img src="%s/img/image-%d.png">`, server.URL, i)
				}
			case "media":
				post["podcast_url"] = server.URL + "/api/v1/audio/upload/episode/src"
				body.WriteString(`<div class="native-video-embed" data-attrs="{&quot;mediaUploadId&quot;:&quot;video-a&quot;}"></div>`)
				body.WriteString(`<div class="native-video-embed" data-attrs="{&quot;mediaUploadId&quot;:&quot;video-b&quot;}"></div>`)
				body.WriteString(audioClipEmbed("clip-a"))
			}
			post["body_html"] = body.String()
			preloads, _ := json.Marshal(map[string]any{"post": post})
			quoted, _ := json.Marshal(string(preloads))
			fmt.Fprintf(w, `<script>window._preloads = JSON.parse(%s)</script>`, quoted)
		case strings.HasPrefix(path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "image bytes")
		case strings.HasPrefix(path, "/api/v1/video/upload/"):
			id, isSrc := strings.CutSuffix(strings.TrimPrefix(path, "/api/v1/video/upload/"), "/src")
			if isSrc {
				http.Redirect(w, r, "/cdn/"+id+".mp4", http.StatusTemporaryRedirect)
				return
			}
			fmt.Fprintf(w, `{"id":%q}`, id)
		case strings.HasPrefix(path, "/api/v1/audio/upload/"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/audio/upload/"), "/src")
			http.Redirect(w, r, "/cdn/"+id+".mp3", http.StatusTemporaryRedirect)
		case strings.HasPrefix(path, "/cdn/"):
			fmt.Fprint(w, "media bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// run returns the requests the command made.
	run := func(t *testing.T, slug string, flags ...string) []request {
		t.Helper()
		mu.Lock()
		requests = nil
		mu.Unlock()
		timeout, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		args := append([]string{"download", "--url", server.URL + "/p/" + slug, "--output", t.TempDir()}, flags...)
		out, err := exec.CommandContext(timeout, binary, args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
		mu.Lock()
		defer mu.Unlock()
		return append([]request(nil), requests...)
	}
	// burst counts the matching requests made within half a second of the first.
	burst := func(requests []request, match func(path string) bool) (burst, total int) {
		var first time.Time
		for _, r := range requests {
			if !match(r.path) {
				continue
			}
			if first.IsZero() {
				first = r.at
			}
			total++
			if r.at.Sub(first) < 500*time.Millisecond {
				burst++
			}
		}
		return burst, total
	}
	isImage := func(path string) bool { return strings.HasPrefix(path, "/img/") }

	t.Run("media shares the post burst by default", func(t *testing.T) {
		inBurst, total := burst(run(t, "gallery", "--rate", "1", "--download-images"), isImage)
		require.Equal(t, images, total)
		// The post page took one of the shared tokens.
		require.Less(t, inBurst, lib.DefaultBurst)
	})
	t.Run("an explicit media rate has its own burst, even if equal", func(t *testing.T) {
		inBurst, total := burst(run(t, "gallery", "--rate", "1", "--media-rate", "1", "--download-images"), isImage)
		require.Equal(t, images, total)
		require.Equal(t, lib.DefaultBurst, inBurst)
	})
	t.Run("media rate speeds up media", func(t *testing.T) {
		inBurst, total := burst(run(t, "gallery", "--rate", "1", "--media-rate", "1000", "--download-images"), isImage)
		require.Equal(t, images, total)
		require.Equal(t, images, inBurst)
	})
	t.Run("publication API requests keep the post rate", func(t *testing.T) {
		requests := run(t, "media", "--rate", "1", "--media-rate", "1000", "--download-videos", "--download-podcasts", "--download-audio-clips")
		// Video metadata, and redirects for videos, the podcast, and the clip.
		inBurst, total := burst(requests, func(path string) bool { return !strings.HasPrefix(path, "/cdn/") })
		require.Equal(t, 1+2*2+1+1, total)
		require.Equal(t, lib.DefaultBurst, inBurst)
		_, transfers := burst(requests, func(path string) bool { return strings.HasPrefix(path, "/cdn/") })
		require.Equal(t, 4, transfers)
	})
	t.Run("negative media rate rejected", func(t *testing.T) {
		out, err := exec.Command(binary, "download", "--url", server.URL+"/p/gallery", "--output", t.TempDir(), "--media-rate", "-1").CombinedOutput()
		require.Error(t, err, "%s", out)
		require.Contains(t, string(out), "media-rate")
	})
}
