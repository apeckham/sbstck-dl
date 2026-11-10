package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexferrari88/sbstck-dl/lib"
	"github.com/stretchr/testify/require"
)

func TestHEICCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "sbstck-dl")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	output, err := exec.Command("go", "build", "-o", binary, "..").CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile("../lib/testdata/preview.heic")
	require.NoError(t, err)
	// A txt-only archive makes no previews; see TestHEICPreviewsOnlyWhenUsed.
	for _, formats := range []string{"html", "md", "html,md,txt"} {
		for _, inBody := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/body-%t", formats, inBody), func(t *testing.T) {
				var posts, images atomic.Int32
				var variant atomic.Int32
				var server *httptest.Server
				server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/sitemap.xml":
						fmt.Fprintf(w, "<urlset><url><loc>%s/p/fixture</loc></url></urlset>", server.URL)
					case "/photo.HEIC":
						images.Add(1)
						switch variant.Load() {
						case 1:
							w.Write(append(append([]byte{}, data...), []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}...))
						case 2:
							fmt.Fprint(w, "corrupt HEIC")
						default:
							w.Write(data)
						}
					case "/p/fixture":
						posts.Add(1)
						body := "<p>Fixture body</p>"
						if inBody {
							body += fmt.Sprintf(`<img src="%s/photo.HEIC" alt="photo">`, server.URL)
						}
						post := lib.Post{Slug: "fixture", Title: "Fixture", PostDate: "2026-01-01T12:00:00Z", CoverImage: server.URL + "/photo.HEIC", BodyHTML: body}
						payload, _ := json.Marshal(lib.PostWrapper{Post: post})
						escaped, _ := json.Marshal(string(payload))
						fmt.Fprintf(w, "<script>window._preloads = JSON.parse(%s)</script>", escaped)
					default:
						http.NotFound(w, r)
					}
				}))
				server.Start()
				defer server.Close()
				dir := t.TempDir()
				selected := strings.Split(formats, ",")
				bodyPrepared := false
				run := func(url string, imagesFlag bool) string {
					t.Helper()
					args := []string{"download", "--url", url, "--create-archive", "--format", formats, "--output", dir, "--images-dir", "assets/nested images", "--rate", "1000"}
					if imagesFlag {
						args = append(args, "--download-images")
					}
					out, err := exec.Command(binary, args...).CombinedOutput()
					require.NoError(t, err, string(out))
					return string(out)
				}
				read := func(name string) []byte {
					t.Helper()
					b, e := os.ReadFile(filepath.Join(dir, name))
					require.NoError(t, e)
					return b
				}
				cover := func() string {
					t.Helper()
					var manifest struct {
						Entries []struct {
							Local  string `json:"local_cover_image"`
							Source string `json:"cover_image"`
						} `json:"entries"`
					}
					require.NoError(t, json.Unmarshal(read(".sbstck-dl-archive-"+selected[0]+".json"), &manifest))
					require.Len(t, manifest.Entries, 1)
					return manifest.Entries[0].Local
				}
				verify := func() string {
					t.Helper()
					path := cover()
					require.Contains(t, path, ".sbstck-previews/")
					require.True(t, strings.HasSuffix(path, "photo.HEIC.png"))
					preview := read(filepath.FromSlash(path))
					img, e := png.Decode(bytes.NewReader(preview))
					require.NoError(t, e)
					require.Equal(t, image.Rect(0, 0, 480, 640), img.Bounds())
					for _, f := range selected {
						post := string(read("20260101_120000_fixture." + f))
						index := string(read("index." + f))
						require.Contains(t, post, "Fixture body")
						if f == "txt" {
							require.NotContains(t, post, ".sbstck-previews/")
							require.NotContains(t, index, ".sbstck-previews/")
						} else {
							require.Contains(t, index, strings.ReplaceAll(path, " ", "%20"))
							if inBody && bodyPrepared {
								destination := (&url.URL{Path: path}).String()
								require.Contains(t, post, destination)
								if f == "md" {
									match := regexp.MustCompile(`!\[photo\]\(([^)]+)\)`).FindStringSubmatch(post)
									require.Len(t, match, 2)
									require.NotContains(t, match[1], " ")
									parsed, e := url.Parse(match[1])
									require.NoError(t, e)
									require.FileExists(t, filepath.Join(dir, filepath.FromSlash(parsed.Path)))
								}
							}
						}
					}
					return path
				}
				run(server.URL, false)
				require.Zero(t, images.Load())
				for _, f := range selected {
					if f != "txt" {
						require.Contains(t, string(read("index."+f)), server.URL+"/photo.HEIC")
					}
				}
				run(server.URL, true)
				require.Equal(t, int32(1), posts.Load())
				require.Equal(t, int32(1), images.Load())
				old := verify()
				// Retained cover-only PNG cache, retry from local original and moved folders.
				run(server.URL, true)
				require.Equal(t, int32(1), images.Load())
				require.Equal(t, old, verify())
				require.NoError(t, os.Remove(filepath.Join(dir, filepath.FromSlash(old))))
				run(server.URL, true)
				require.Equal(t, int32(1), images.Load())
				verify()
				moved := filepath.Join(t.TempDir(), "moved")
				require.NoError(t, os.Rename(dir, moved))
				dir = moved
				run(server.URL, true)
				verify()
				require.Equal(t, int32(1), posts.Load())
				require.Equal(t, int32(1), images.Load())
				// Explicit body download should share the newly produced preview with cover.
				if inBody {
					variant.Store(1)
					bodyPrepared = true
					run(server.URL+"/p/fixture", true)
					require.Equal(t, int32(2), images.Load())
					changed := verify()
					require.NotEqual(t, old, changed)
					require.FileExists(t, filepath.Join(dir, filepath.FromSlash(old)))
					variant.Store(2)
					warnings := run(server.URL+"/p/fixture", true)
					require.Contains(t, warnings, "Warning:")
					require.Equal(t, int32(3), images.Load(), "failed body cover must not GET twice")
					require.Empty(t, cover())
					for _, f := range selected {
						if f != "txt" {
							require.Contains(t, string(read("index."+f)), server.URL+"/photo.HEIC")
							require.Contains(t, string(read("20260101_120000_fixture."+f)), server.URL+"/photo.HEIC")
						}
					}
					require.FileExists(t, filepath.Join(dir, filepath.FromSlash(changed)))
					variant.Store(0)
					run(server.URL+"/p/fixture", true)
					verify()
					require.Equal(t, int32(4), images.Load())
				}
				// Original remains available and byte-for-byte faithful after conversion.
				var originals []string
				require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
					if e != nil {
						return e
					}
					if !d.IsDir() && strings.HasSuffix(path, "photo.HEIC") {
						originals = append(originals, path)
					}
					return nil
				}))
				require.NotEmpty(t, originals)
				for _, path := range originals {
					b, e := os.ReadFile(path)
					require.NoError(t, e)
					require.Equal(t, data, b)
				}
			})
		}
	}
}

func TestHEICPreviewsOnlyWhenUsed(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "sbstck-dl")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	output, err := exec.Command("go", "build", "-o", binary, "..").CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile("../lib/testdata/preview.heic")
	require.NoError(t, err)
	var sharedCover atomic.Bool
	// Start after assignment, so the handler's read of server.URL is ordered.
	var server *httptest.Server
	server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sitemap.xml":
			fmt.Fprintf(w, "<urlset><url><loc>%s/p/fixture</loc></url></urlset>", server.URL)
		case "/photo.HEIC", "/cover.HEIC":
			w.Write(data)
		case "/p/fixture":
			// A separate cover exercises the archive's own cover download.
			cover := server.URL + "/cover.HEIC"
			if sharedCover.Load() {
				cover = server.URL + "/photo.HEIC"
			}
			post := lib.Post{Slug: "fixture", Title: "Fixture", PostDate: "2026-01-01T12:00:00Z", CoverImage: cover, BodyHTML: fmt.Sprintf(`<p>Fixture body</p><img src="%s/photo.HEIC" alt="photo">`, server.URL)}
			payload, _ := json.Marshal(lib.PostWrapper{Post: post})
			escaped, _ := json.Marshal(string(payload))
			fmt.Fprintf(w, "<script>window._preloads = JSON.parse(%s)</script>", escaped)
		default:
			http.NotFound(w, r)
		}
	}))
	server.Start()
	defer server.Close()
	// Originals keep their served names; previews add a .png suffix.
	walk := func(dir, name string) (originals, previews []string) {
		require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if strings.Contains(path, ".sbstck-previews") && d.Name() == name+".png" {
				previews = append(previews, path)
			} else if d.Name() == name {
				originals = append(originals, path)
			}
			return nil
		}))
		return originals, previews
	}
	for _, tc := range []struct {
		formats  string
		archive  bool
		previews bool
	}{
		{"txt", false, false},
		{"html", false, true},
		{"md", false, true},
		{"html,txt", false, true},
		// index.txt has no image references, so its cover needs no preview.
		{"txt", true, false},
		{"html", true, true},
	} {
		t.Run(fmt.Sprintf("%s/archive-%t", tc.formats, tc.archive), func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"download", "--url", server.URL + "/p/fixture", "--format", tc.formats, "--output", dir, "--download-images", "--rate", "1000"}
			if tc.archive {
				args = append(args, "--create-archive")
			}
			out, err := exec.Command(binary, args...).CombinedOutput()
			require.NoError(t, err, string(out))
			require.NotContains(t, string(out), "Warning")
			names := []string{"photo.HEIC"}
			if tc.archive {
				names = append(names, "cover.HEIC")
			}
			for _, name := range names {
				originals, previews := walk(dir, name)
				require.Len(t, originals, 1)
				original, err := os.ReadFile(originals[0])
				require.NoError(t, err)
				require.Equal(t, data, original, "original kept as served")
				if tc.previews {
					require.NotEmpty(t, previews)
				} else {
					require.Empty(t, previews)
				}
			}
		})
	}
	// Adding txt beside an existing HTML post writes only txt, but the HTML
	// archive cover reuses the body image, so that image still needs a preview.
	t.Run("txt-added-to-html-archive", func(t *testing.T) {
		sharedCover.Store(true)
		defer sharedCover.Store(false)
		dir := t.TempDir()
		for _, extra := range [][]string{{"--format", "html"}, {"--format", "html,txt", "--download-images"}} {
			args := append([]string{"download", "--url", server.URL, "--create-archive", "--output", dir, "--rate", "1000"}, extra...)
			out, err := exec.Command(binary, args...).CombinedOutput()
			require.NoError(t, err, string(out))
			require.NotContains(t, string(out), "Warning")
		}
		_, previews := walk(dir, "photo.HEIC")
		require.Len(t, previews, 1)
		index, err := os.ReadFile(filepath.Join(dir, "index.html"))
		require.NoError(t, err)
		require.Contains(t, string(index), "photo.HEIC.png")
		post, err := os.ReadFile(filepath.Join(dir, "20260101_120000_fixture.html"))
		require.NoError(t, err)
		require.NotContains(t, string(post), "photo.HEIC.png", "existing HTML post is not rewritten")
	})
}
