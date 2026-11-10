package lib

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Substack renders image galleries client-side: the post HTML holds an empty
// div whose images, caption, and a static grid image are listed only in its
// data-attrs JSON.
type galleryAttrs struct {
	Gallery struct {
		Images []struct {
			Src string `json:"src"`
		} `json:"images"`
		Caption string `json:"caption"`
		Alt     string `json:"alt"`
		Static  *struct {
			Src string `json:"src"`
		} `json:"staticGalleryImage"`
	} `json:"gallery"`
}

// emptyGalleryPattern matches a gallery placeholder that has not been rendered.
var emptyGalleryPattern = regexp.MustCompile(`<div\b[^>]*\bimage-gallery-embed\b[^>]*>\s*</div>`)

// galleryImageURLs lists a gallery's image URLs in order, then its static grid
// image, without duplicates.
func galleryImageURLs(dataAttrs string) []string {
	var attrs galleryAttrs
	if err := json.Unmarshal([]byte(dataAttrs), &attrs); err != nil {
		return nil
	}
	var urls []string
	seen := make(map[string]bool)
	add := func(src string) {
		if src != "" && !seen[src] {
			seen[src] = true
			urls = append(urls, src)
		}
	}
	for _, image := range attrs.Gallery.Images {
		add(image.Src)
	}
	if attrs.Gallery.Static != nil {
		add(attrs.Gallery.Static.Src)
	}
	return urls
}

// galleryElements returns download entries for the gallery images in doc.
func galleryElements(doc *goquery.Document) []ImageElement {
	var elements []ImageElement
	doc.Find(".image-gallery-embed[data-attrs]").Each(func(i int, s *goquery.Selection) {
		for _, src := range galleryImageURLs(s.AttrOr("data-attrs", "")) {
			elements = append(elements, ImageElement{BestURL: src, AllURLs: []string{src}})
		}
	})
	return elements
}

// rewriteGalleryAttrs replaces gallery image URLs in data-attrs JSON, keeping
// every other field as it is.
func rewriteGalleryAttrs(dataAttrs string, urlToRelPath map[string]string) string {
	var attrs map[string]any
	if err := json.Unmarshal([]byte(dataAttrs), &attrs); err != nil {
		return dataAttrs
	}
	gallery, ok := attrs["gallery"].(map[string]any)
	if !ok {
		return dataAttrs
	}
	replace := func(image any) {
		if m, ok := image.(map[string]any); ok {
			if src, ok := m["src"].(string); ok {
				if rel, found := urlToRelPath[src]; found {
					m["src"] = rel
				}
			}
		}
	}
	if images, ok := gallery["images"].([]any); ok {
		for _, image := range images {
			replace(image)
		}
	}
	replace(gallery["staticGalleryImage"])
	updated, err := json.Marshal(attrs)
	if err != nil {
		return dataAttrs
	}
	return string(updated)
}

// RenderGalleries fills empty gallery placeholders with their images and
// caption, so HTML, Markdown, and text output show them. The static grid image
// is not shown because it repeats the images. Rendered galleries are left as
// they are, so rendering twice changes nothing.
func RenderGalleries(postHTML string) string {
	return emptyGalleryPattern.ReplaceAllStringFunc(postHTML, func(placeholder string) string {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(placeholder))
		if err != nil {
			return placeholder
		}
		var attrs galleryAttrs
		if err := json.Unmarshal([]byte(doc.Find("div").AttrOr("data-attrs", "")), &attrs); err != nil || len(attrs.Gallery.Images) == 0 {
			return placeholder
		}
		var b strings.Builder
		b.WriteString(`<figure class="image-gallery">`)
		for _, image := range attrs.Gallery.Images {
			fmt.Fprintf(&b, `<img src="%s" alt="%s">`, html.EscapeString(image.Src), html.EscapeString(attrs.Gallery.Alt))
		}
		if attrs.Gallery.Caption != "" {
			fmt.Fprintf(&b, `<figcaption>%s</figcaption>`, html.EscapeString(attrs.Gallery.Caption))
		}
		b.WriteString(`</figure>`)
		closing := strings.LastIndex(placeholder, "</div>")
		return placeholder[:closing] + b.String() + placeholder[closing:]
	})
}
