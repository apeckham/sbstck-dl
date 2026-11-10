package lib

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDownloadImagesInParallel(t *testing.T) {
	const images, workers, delay = 12, 3, 100 * time.Millisecond
	var mu sync.Mutex
	inFlight, peak := 0, 0
	var log []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		log = append(log, "start "+r.URL.Path)
		mu.Unlock()
		time.Sleep(delay)
		mu.Lock()
		inFlight--
		log = append(log, "end "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		w.Write(testImageData)
		// Same-named images differ by their trailing marker.
		fmt.Fprint(w, r.URL.Path)
	}))
	defer server.Close()
	fetcher := NewFetcher(WithRatePerSecond(1000), WithBurst(1000), WithMaxWorkers(workers))
	reset := func() {
		mu.Lock()
		defer mu.Unlock()
		inFlight, peak, log = 0, 0, nil
	}

	t.Run("downloads concurrently within the worker limit", func(t *testing.T) {
		reset()
		var html strings.Builder
		for i := 0; i < images; i++ {
			fmt.Fprintf(&html, `<img src="%s/img-%02d.png">`, server.URL, i)
		}
		dir := t.TempDir()
		start := time.Now()
		result, err := NewImageDownloader(fetcher, dir, "images", ImageQualityHigh).DownloadImages(context.Background(), html.String(), "post")
		elapsed := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, images, result.Success)
		// Sequential downloads would need images*delay.
		require.Less(t, elapsed, time.Duration(images)*delay/2)
		mu.Lock()
		require.Equal(t, workers, peak)
		mu.Unlock()
		// Results keep document order, and every reference is rewritten.
		for i, image := range result.Images {
			require.Equal(t, fmt.Sprintf("%s/img-%02d.png", server.URL, i), image.OriginalURL)
			require.Contains(t, result.UpdatedHTML, fmt.Sprintf("images/post/img-%02d.png", i))
		}
		require.NotContains(t, result.UpdatedHTML, server.URL)
	})

	t.Run("same filename downloads one at a time in document order", func(t *testing.T) {
		// Names differing only in case are the same file on case-insensitive
		// file systems.
		html := fmt.Sprintf(`<img src="%[1]s/a/photo.png"><img src="%[1]s/other.png"><img src="%[1]s/b/Photo.png"><img src="%[1]s/c/photo.png">`, server.URL)
		for i := 0; i < 5; i++ {
			reset()
			dir := t.TempDir()
			result, err := NewImageDownloader(fetcher, dir, "images", ImageQualityHigh).DownloadImages(context.Background(), html, "post")
			require.NoError(t, err)
			require.Equal(t, 4, result.Success)
			data, err := os.ReadFile(filepath.Join(dir, "images", "post", "photo.png"))
			require.NoError(t, err)
			// The later image wins, as it did when downloads ran one at a time.
			require.True(t, strings.HasSuffix(string(data), "/c/photo.png"), string(data[len(testImageData):]))
			mu.Lock()
			var group []string
			for _, entry := range log {
				if strings.Contains(strings.ToLower(entry), "photo.png") {
					group = append(group, entry)
				}
			}
			mu.Unlock()
			require.Equal(t, []string{
				"start /a/photo.png", "end /a/photo.png",
				"start /b/Photo.png", "end /b/Photo.png",
				"start /c/photo.png", "end /c/photo.png",
			}, group)
		}
	})
}
