package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteToFileWithImagesNoImagesCompatibility(t *testing.T) {
	post := Post{Title: "Title", BodyHTML: "<p>Body</p>", CanonicalUrl: "https://example.com/p/post"}
	for _, format := range []string{"html", "md", "txt", "json"} {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "post."+format)
			result, err := post.WriteToFileWithImages(context.Background(), path, format, true, false, ImageQualityHigh, "images", false, nil, "files", nil)
			require.NoError(t, err)
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, string(body), result.UpdatedHTML)
			require.NotNil(t, result.Images)
			require.Empty(t, result.Images)
		})
	}
}

// JSON is the serialized Post: requested media is saved, but body_html stays
// as served instead of pointing at the local copies.
func TestWriteToFileWithImagesJSONKeepsServedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image.jpg":
			fmt.Fprint(w, "image bytes")
		case "/attachment.pdf":
			fmt.Fprint(w, "attachment bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	post := Post{Slug: "post", Title: "Title", CanonicalUrl: server.URL + "/p/post",
		BodyHTML: fmt.Sprintf(`<p>Body</p><img src="%s/image.jpg"><a class="file-embed-button wide" href="%s/attachment.pdf">Attachment</a>`, server.URL, server.URL)}
	dir := t.TempDir()
	path := filepath.Join(dir, "post.json")
	_, err := post.WriteToFileWithImages(context.Background(), path, "json", true, true, ImageQualityHigh, "images", true, nil, "files", NewFetcher())
	require.NoError(t, err)
	image, err := os.ReadFile(filepath.Join(dir, "images", "post", "image.jpg"))
	require.NoError(t, err)
	require.Equal(t, "image bytes", string(image))
	attachment, err := os.ReadFile(filepath.Join(dir, "files", "post", "attachment.pdf"))
	require.NoError(t, err)
	require.Equal(t, "attachment bytes", string(attachment))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var saved Post
	require.NoError(t, json.Unmarshal(body, &saved))
	require.Equal(t, post, saved)
}

func TestCanceledMediaNeverPublishesPost(t *testing.T) {
	post := Post{Title: "Title", BodyHTML: "<p>Body</p>"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "post.html")
	require.NoError(t, os.WriteFile(path, []byte("prior valid post"), 0600))
	_, err := post.WriteToFileWithImages(ctx, path, "html", false, false, ImageQualityHigh, "images", false, nil, "files", nil)
	require.True(t, errors.Is(err, context.Canceled))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "prior valid post", string(body))
}
