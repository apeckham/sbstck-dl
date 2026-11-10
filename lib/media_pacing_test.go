package lib

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestFetcherMediaLimiter(t *testing.T) {
	t.Run("media shares the page limiter by default", func(t *testing.T) {
		f := NewFetcher(WithRatePerSecond(1))
		require.Nil(t, f.MediaLimiter)
		require.Same(t, f.RateLimiter, f.mediaLimiter())
	})
	t.Run("an explicit media rate gets its own burst, even if equal", func(t *testing.T) {
		f := NewFetcher(WithRatePerSecond(1), WithMediaRatePerSecond(1))
		require.NotNil(t, f.MediaLimiter)
		require.NotSame(t, f.RateLimiter, f.MediaLimiter)
		require.Equal(t, rate.Limit(1), f.MediaLimiter.Limit())
		require.Equal(t, DefaultBurst, f.MediaLimiter.Burst())
		now := time.Now()
		require.True(t, f.RateLimiter.AllowN(now, DefaultBurst))
		require.True(t, f.MediaLimiter.AllowN(now, DefaultBurst))
	})
}

// Each kind of download takes page tokens for publication API requests and
// redirect resolution, and media tokens for the file transfers.
func TestMediaPacing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
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
		case strings.HasPrefix(path, "/img/"):
			w.Header().Set("Content-Type", "image/png")
			w.Write(testImageData)
		case path == "/files/notes.pdf":
			fmt.Fprint(w, "pdf bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	origin, err := url.Parse(server.URL)
	require.NoError(t, err)
	ctx := context.Background()

	// Limiters that do not refill during the test, so used tokens are exact.
	const tokens = 100
	newFetcher := func() *Fetcher {
		f := NewFetcher()
		f.RateLimiter = rate.NewLimiter(rate.Every(time.Hour), tokens)
		f.MediaLimiter = rate.NewLimiter(rate.Every(time.Hour), tokens)
		return f
	}
	used := func(l *rate.Limiter) int {
		return int(tokens - l.Tokens() + 0.5)
	}

	tests := []struct {
		name        string
		download    func(t *testing.T, f *Fetcher, dir string)
		page, media int
	}{
		{"videos", func(t *testing.T, f *Fetcher, dir string) {
			var body strings.Builder
			for i := 0; i < 4; i++ {
				body.WriteString(nativeVideoEmbed(fmt.Sprintf("video-%d", i)))
			}
			post := Post{CanonicalUrl: server.URL + "/p/post", BodyHTML: body.String()}
			result, err := NewVideoDownloader(f, dir, "videos").DownloadVideos(ctx, post, "post")
			require.NoError(t, err)
			require.Equal(t, 4, result.Success)
		}, 8, 4},
		{"podcast", func(t *testing.T, f *Fetcher, dir string) {
			post := Post{CanonicalUrl: server.URL + "/p/post", PodcastUrl: server.URL + "/api/v1/audio/upload/episode/src"}
			_, err := NewPodcastDownloader(f, dir, "podcasts").DownloadPodcast(ctx, post, "post")
			require.NoError(t, err)
		}, 1, 1},
		{"audio clips", func(t *testing.T, f *Fetcher, dir string) {
			post := Post{BodyHTML: audioClipPlaceholder("clip-a", 0) + audioClipPlaceholder("clip-b", 0)}
			result, err := NewAudioClipDownloader(f, dir, "clips").DownloadAudioClips(ctx, post, "post", origin)
			require.NoError(t, err)
			require.Equal(t, 2, result.Success)
		}, 2, 2},
		{"images", func(t *testing.T, f *Fetcher, dir string) {
			html := fmt.Sprintf(`<img src="%[1]s/img/a.png"><img src="%[1]s/img/b.png"><img src="%[1]s/img/c.png">`, server.URL)
			result, err := NewImageDownloader(f, dir, "images", ImageQualityHigh).DownloadImages(ctx, html, "post")
			require.NoError(t, err)
			require.Equal(t, 3, result.Success)
		}, 0, 3},
		{"archive cover", func(t *testing.T, f *Fetcher, dir string) {
			info := NewImageDownloader(f, dir, "images", ImageQualityHigh).downloadCoverImage(ctx, server.URL+"/img/cover.png", "post")
			require.NoError(t, info.Error)
			require.True(t, info.Success)
		}, 0, 1},
		{"attachments stay on the page rate", func(t *testing.T, f *Fetcher, dir string) {
			html := fmt.Sprintf(`<a class="file-embed-button wide" href="%s/files/notes.pdf">Download</a>`, server.URL)
			result, err := NewFileDownloader(f, dir, "files", nil).DownloadFiles(ctx, html, "post")
			require.NoError(t, err)
			require.Equal(t, 1, result.Success)
		}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFetcher()
			tt.download(t, f, t.TempDir())
			require.Equal(t, tt.page, used(f.RateLimiter), "page tokens")
			require.Equal(t, tt.media, used(f.MediaLimiter), "media tokens")
		})
	}
}
