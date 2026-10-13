package screening

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/emersion/go-webdav"
)

// listETags returns path → ETag of the vCards in an address book with a
// minimal PROPFIND (Depth 1). It is deliberately lenient: servers differ
// in which properties they return, and only the ETag is needed here.
func listETags(ctx context.Context, hc webdav.HTTPClient, base *url.URL, book string) (map[string]string, error) {
	const body = `<?xml version="1.0" encoding="utf-8"?>` +
		`<d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:resourcetype/></d:prop></d:propfind>`
	u := *base
	u.Path, u.RawPath = book, ""
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", u.String(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("PROPFIND %s: %d %s", book, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var ms struct {
		Responses []struct {
			Href     string `xml:"href"`
			Propstat []struct {
				Prop struct {
					ETag         string `xml:"getetag"`
					ResourceType struct {
						Collection *struct{} `xml:"collection"`
					} `xml:"resourcetype"`
				} `xml:"prop"`
				Status string `xml:"status"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("PROPFIND %s: %w", book, err)
	}
	out := map[string]string{}
	for _, r := range ms.Responses {
		h, err := url.Parse(strings.TrimSpace(r.Href))
		if err != nil {
			continue
		}
		p := h.Path
		if strings.TrimSuffix(p, "/") == strings.TrimSuffix(book, "/") {
			continue // the collection itself
		}
		var etag string
		dir := false
		for _, ps := range r.Propstat {
			if ps.Status != "" && !strings.Contains(ps.Status, " 200") {
				continue
			}
			if ps.Prop.ETag != "" {
				etag = strings.TrimSpace(ps.Prop.ETag)
			}
			if ps.Prop.ResourceType.Collection != nil {
				dir = true
			}
		}
		if !dir {
			out[p] = etag
		}
	}
	return out, nil
}
