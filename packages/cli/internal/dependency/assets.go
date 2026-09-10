package dependency

import (
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"path"
	"strings"
)

// SVG assets are XML href references, resolved relative to the SVG itself.
// The standard decoder does not retrieve DTDs or expand external entities.
func svgAssets(d *discoverer, source string) {
	content, err := d.readText(source)
	if generated, ok := d.generated[source]; ok {
		content = generated.content
		err = nil
	}
	if err != nil {
		d.addDiagnostic(source, 0, "", "", "unavailable", err.Error())
		return
	}
	decoder := xml.NewDecoder(strings.NewReader(content))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			d.addDiagnostic(source, 0, "", "", "unsupported", "cannot parse SVG companion references: "+err.Error())
			return
		}
		element, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		for _, attr := range element.Attr {
			if attr.Name.Space == "http://www.w3.org/XML/1998/namespace" && attr.Name.Local == "base" {
				d.addDiagnostic(
					source,
					0,
					"xml:base",
					attr.Value,
					"unsupported",
					"SVG XML base overrides require explicit manifest selection",
				)
				return
			}
		}
		if element.Name.Local != "image" && element.Name.Local != "use" && element.Name.Local != "feImage" {
			continue
		}
		for _, attr := range element.Attr {
			if attr.Name.Local != "href" {
				continue
			}
			uri, err := url.Parse(strings.TrimSpace(attr.Value))
			if err != nil {
				d.addDiagnostic(source, 0, "href", attr.Value, "unsupported", "invalid SVG resource URI")
				continue
			}
			if uri.Scheme == "file" {
				d.addDiagnostic(source, 0, "href", attr.Value, "outside_root", "SVG resource escapes the project root")
				continue
			}
			if uri.IsAbs() || uri.Host != "" || uri.Path == "" {
				continue
			}
			if strings.HasPrefix(uri.Path, "/") {
				d.addDiagnostic(source, 0, "href", attr.Value, "outside_root", "SVG resource escapes the project root")
				continue
			}
			d.consumeReference(
				source,
				0,
				"href",
				path.Join(path.Dir(source), uri.Path),
				referenceRule{extensions: []string{""}, rootRelative: true},
			)
		}
	}
}
