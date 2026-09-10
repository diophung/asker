package mimeguard

import "testing"

// The flat denylist that shipped first was spelling-dependent: it named
// application/xhtml+xml and image/svg+xml but let the rest of the open-ended
// "+xml" structured-suffix family through. Browsers render several of those
// as documents that honor XML processing instructions, and text/xsl drives
// XSLT which can emit HTML into the serving origin.
func TestSanitizeNeutralizesRenderableFamilies(t *testing.T) {
	for _, in := range []string{
		// Named entries.
		"text/html", "TEXT/HTML", "text/html; charset=utf-8", "  text/html  ",
		"text/x-html",
		"application/xhtml+xml", "application/xhtml",
		"image/svg+xml", "image/svg", "application/svg+xml",
		"application/xml", "text/xml", "application/xml-dtd",
		"text/xml-external-parsed-entity",
		"application/xslt+xml", "text/xsl",
		"text/javascript", "application/javascript", "application/x-javascript",
		"application/ecmascript", "text/ecmascript",
		"application/x-shockwave-flash",
		// The "+xml" family the first denylist missed.
		"application/rss+xml", "application/atom+xml",
		"application/mathml+xml", "application/vnd.mozilla.xul+xml",
		"application/vnd.wap.xhtml+xml", "image/svg+xml; charset=utf-8",
		"APPLICATION/RSS+XML",
		// multipart streams render as a sequence of documents.
		"multipart/x-mixed-replace", "multipart/form-data",
	} {
		if got := SanitizeContentType(in); got != DefaultContentType {
			t.Errorf("SanitizeContentType(%q) = %q, want %q", in, got, DefaultContentType)
		}
	}
}

func TestSanitizeRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"", "   ", "not-a-media-type", "text/html/extra",
		"text/plain; charset", "image/png\x00text/html", "image/png, text/html",
		"image/", "/png",
	} {
		if got := SanitizeContentType(in); got != DefaultContentType {
			t.Errorf("SanitizeContentType(%q) = %q, want %q", in, got, DefaultContentType)
		}
	}
}

// The denylist approach exists so ordinary user files keep their real type —
// an allowlist would degrade all of these to octet-stream and break doc-type
// classification and text extraction downstream.
func TestSanitizePreservesBenignTypes(t *testing.T) {
	cases := map[string]string{
		"image/png":                 "image/png",
		"IMAGE/PNG":                 "image/png",
		"image/jpeg":                "image/jpeg",
		"image/webp":                "image/webp",
		"video/mp4":                 "video/mp4",
		"audio/mpeg":                "audio/mpeg",
		"application/pdf":           "application/pdf",
		"text/plain":                "text/plain",
		"text/plain; charset=utf-8": "text/plain; charset=utf-8",
		"text/markdown":             "text/markdown",
		"text/csv":                  "text/csv",
		"application/json":          "application/json",
		"application/zip":           "application/zip",
		"application/octet-stream":  "application/octet-stream",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	}
	for in, want := range cases {
		if got := SanitizeContentType(in); got != want {
			t.Errorf("SanitizeContentType(%q) = %q, want %q", in, got, want)
		}
	}
}
