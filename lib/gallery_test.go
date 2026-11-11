package lib

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// galleryEmbed returns Substack's gallery markup: an empty div whose images
// are listed only in its data-attrs JSON.
func galleryEmbed(caption, static string, srcs ...string) string {
	var images []string
	for _, src := range srcs {
		images = append(images, fmt.Sprintf(`{"type":"image/jpeg","src":%q}`, src))
	}
	attrs := fmt.Sprintf(`{"gallery":{"images":[%s],"caption":%q,"alt":"","staticGalleryImage":{"type":"image/png","src":%q}},"isEditorNode":true}`,
		strings.Join(images, ","), caption, static)
	return `<div class="image-gallery-embed" data-attrs="` + html.EscapeString(attrs) + `"></div>`
}

func TestGalleryImagesAreDownloaded(t *testing.T) {
	// Images may download concurrently, so the handler guards requests.
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		w.Write(testImageData)
	}))
	defer server.Close()
	body := `<p>Before</p>` + galleryEmbed("Our trip", server.URL+"/grid.png", server.URL+"/a.jpeg", server.URL+"/b.jpeg", server.URL+"/a.jpeg") +
		`<img src="` + server.URL + `/b.jpeg">`
	dir := t.TempDir()
	result, err := NewImageDownloader(NewFetcher(WithRatePerSecond(1000)), dir, "images", ImageQualityHigh).DownloadImages(context.Background(), body, "post")
	require.NoError(t, err)

	for _, name := range []string{"a.jpeg", "b.jpeg", "grid.png"} {
		_, err := os.Stat(filepath.Join(dir, "images", "post", name))
		require.NoError(t, err, name)
		mu.Lock()
		joined := strings.Join(requests, " ")
		mu.Unlock()
		require.Equal(t, 1, strings.Count(joined, "/"+name), "each image is fetched once")
	}
	require.Equal(t, 3, result.Success)
	attrs := html.UnescapeString(result.UpdatedHTML)
	require.Contains(t, attrs, `"src":"images/post/a.jpeg"`)
	require.Contains(t, attrs, `"src":"images/post/grid.png"`)
	require.NotContains(t, attrs, server.URL+"/a.jpeg")
	require.Contains(t, attrs, `"caption":"Our trip"`)
}

func TestRenderGalleries(t *testing.T) {
	body := `<p>Before</p>` + galleryEmbed(`Tom & "Jerry"`, "images/grid.png", "images/a.jpeg", "images/b.jpeg") + `<p>After</p>`
	rendered := RenderGalleries(body)
	require.Contains(t, rendered, `<img src="images/a.jpeg"`)
	require.Contains(t, rendered, `<img src="images/b.jpeg"`)
	require.NotContains(t, rendered, `<img src="images/grid.png"`, "the static grid duplicates the images")
	require.Contains(t, rendered, `<figcaption>Tom &amp; &#34;Jerry&#34;</figcaption>`)
	require.Less(t, strings.Index(rendered, "Before"), strings.Index(rendered, "images/a.jpeg"))
	require.Less(t, strings.Index(rendered, "images/b.jpeg"), strings.Index(rendered, "After"))
	require.Equal(t, rendered, RenderGalleries(rendered), "rendering twice changes nothing")

	post := Post{Title: "t", BodyHTML: body}
	md, err := post.ToMD(false)
	require.NoError(t, err)
	require.Contains(t, md, "](images/a.jpeg)")
	require.Contains(t, md, `Tom & "Jerry"`)
	require.Contains(t, post.ToText(false), `Tom & "Jerry"`)
	require.Contains(t, post.ToHTML(false), `<img src="images/b.jpeg"`)
	require.Equal(t, body, post.BodyHTML, "the post body itself is not modified")
}
