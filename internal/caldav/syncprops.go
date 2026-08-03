package caldav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	nsDAV            = "DAV:"
	nsCalendarServer = "http://calendarserver.org/ns/"
	nsCalDAV         = "urn:ietf:params:xml:ns:caldav"
)

// SyncPropsStub answers the collection properties that make sync-collection
// discoverable: DAV:sync-token, DAV:supported-report-set, and the older
// CS:getctag. go-webdav emits none of them — sync-token and getctag come back
// inside its 404 propstat, and supported-report-set is never emitted at all, so
// clients currently have no way to learn that any report is supported. Neither
// is reachable through the Backend interface.
//
// Three paths, in increasing order of intrusiveness:
//
//   - The request names none of ours → passed through untouched.
//   - It names only ours → answered entirely here; the library is never called.
//     This is the common case (Cfait asks for exactly one property).
//   - It mixes ours with the library's → ours are stripped from the request so
//     the library doesn't 404 them, and its response is spliced rather than
//     rewritten. Additive splicing keeps every byte the library produced,
//     which matters because re-encoding a multistatus can disturb namespace
//     prefixes that some clients are particular about.
//
// allprop is deliberately left alone: RFC 6578 §3 says DAV:sync-token is not
// returned for it.
func SyncPropsStub(next http.Handler, store SyncStore, backend *Backend) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || store == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		ours, theirs := partitionPropfind(body)
		if len(ours) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		token, err := store.MaxUpdatedAtAll(r.Context())
		if err != nil {
			http.Error(w, "sync token", http.StatusInternalServerError)
			return
		}
		propsXML := renderSyncProps(ours, token)

		// Nothing left for the library to answer — synthesize the whole thing.
		if len(theirs) == 0 {
			writeSinglePropstat(w, r.URL.Path, propsXML)
			return
		}

		// Hand the library only what it can answer, then splice ours in.
		r.Body = io.NopCloser(strings.NewReader(rewritePropfind(theirs)))
		rec := &bufferedResponse{header: http.Header{}}
		next.ServeHTTP(rec, r)

		merged, ok := splicePropstat(rec.body.Bytes(), r.URL.Path, propsXML)
		if !ok {
			// Couldn't find the collection's response element. Emitting the
			// library's answer unchanged loses our properties but keeps a valid
			// document, which is the better failure.
			rec.flushTo(w)
			return
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write(merged)
	})
}

// partitionPropfind splits the requested properties into the ones this
// middleware owns and the ones the library should answer. Returns empty
// slices for allprop/propname requests, which are passed through untouched.
func partitionPropfind(body []byte) (ours, theirs []xml.Name) {
	var doc struct {
		XMLName  xml.Name
		AllProp  *struct{} `xml:"DAV: allprop"`
		PropName *struct{} `xml:"DAV: propname"`
		Prop     struct {
			Props []xmlPropName `xml:",any"`
		} `xml:"DAV: prop"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, nil
	}
	if doc.XMLName.Space != nsDAV || doc.XMLName.Local != "propfind" {
		return nil, nil
	}
	if doc.AllProp != nil || doc.PropName != nil {
		return nil, nil
	}
	for _, p := range doc.Prop.Props {
		if isSyncProp(p.XMLName) {
			ours = append(ours, p.XMLName)
		} else {
			theirs = append(theirs, p.XMLName)
		}
	}
	return ours, theirs
}

func isSyncProp(n xml.Name) bool {
	switch {
	case n.Space == nsDAV && n.Local == "sync-token":
		return true
	case n.Space == nsDAV && n.Local == "supported-report-set":
		return true
	case n.Space == nsCalendarServer && n.Local == "getctag":
		return true
	}
	return false
}

// renderSyncProps builds the <prop> children for the properties we own.
// getctag and sync-token deliberately carry the same value: both are opaque
// collection change tokens, and a client seeing them disagree could bootstrap
// against the wrong one.
func renderSyncProps(ours []xml.Name, token int64) string {
	var b strings.Builder
	value := xmlEscape(formatSyncToken(token))
	for _, n := range ours {
		switch {
		case n.Space == nsCalendarServer && n.Local == "getctag":
			fmt.Fprintf(&b, `        <CS:getctag xmlns:CS="%s">%s</CS:getctag>`+"\n", nsCalendarServer, value)
		case n.Space == nsDAV && n.Local == "sync-token":
			fmt.Fprintf(&b, "        <D:sync-token>%s</D:sync-token>\n", value)
		case n.Space == nsDAV && n.Local == "supported-report-set":
			fmt.Fprintf(&b, `        <D:supported-report-set>
          <D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report>
          <D:supported-report><D:report><C:calendar-query xmlns:C="%s"/></D:report></D:supported-report>
          <D:supported-report><D:report><C:calendar-multiget xmlns:C="%s"/></D:report></D:supported-report>
        </D:supported-report-set>`+"\n", nsCalDAV, nsCalDAV)
		}
	}
	return b.String()
}

// rewritePropfind rebuilds a PROPFIND body carrying only the given properties.
func rewritePropfind(props []xml.Name) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<propfind xmlns="DAV:"><prop>`)
	for _, n := range props {
		if n.Space == "" {
			fmt.Fprintf(&b, `<%s/>`, xmlEscape(n.Local))
			continue
		}
		fmt.Fprintf(&b, `<%s xmlns="%s"/>`, xmlEscape(n.Local), xmlEscape(n.Space))
	}
	b.WriteString(`</prop></propfind>`)
	return b.String()
}

// propstatBlock wraps rendered properties in a 200 propstat.
func propstatBlock(propsXML string) string {
	return "    <D:propstat xmlns:D=\"DAV:\">\n      <D:prop>\n" + propsXML +
		"      </D:prop>\n      <D:status>HTTP/1.1 200 OK</D:status>\n    </D:propstat>\n"
}

// writeSinglePropstat emits a complete multistatus for the case where every
// requested property is ours.
func writeSinglePropstat(w http.ResponseWriter, href, propsXML string) {
	body := `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<D:multistatus xmlns:D="DAV:">` + "\n" +
		"  <D:response>\n    <D:href>" + xmlEscape(href) + "</D:href>\n" +
		propstatBlock(propsXML) +
		"  </D:response>\n</D:multistatus>"

	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, body)
}

// splicePropstat inserts a propstat block just before the closing tag of the
// <response> whose <href> matches the given path, leaving every other byte of
// the document exactly as the library produced it.
//
// The insertion point is found by streaming the document and recording the
// decoder offset immediately before each token, so the splice lands at a real
// byte boundary without re-encoding anything. Returns false if no matching
// response element is found.
func splicePropstat(doc []byte, href, propsXML string) ([]byte, bool) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	depth := 0
	inResponse := false
	matched := false
	responseDepth := 0
	var pendingHref strings.Builder
	captureHref := false
	prevOffset := int64(0)

	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Space == nsDAV && t.Name.Local == "response" && !inResponse {
				inResponse, matched, responseDepth = true, false, depth
				pendingHref.Reset()
			} else if inResponse && t.Name.Space == nsDAV && t.Name.Local == "href" && depth == responseDepth+1 {
				captureHref = true
				pendingHref.Reset()
			}
		case xml.CharData:
			if captureHref {
				pendingHref.Write(t)
			}
		case xml.EndElement:
			if captureHref && t.Name.Space == nsDAV && t.Name.Local == "href" {
				captureHref = false
				matched = strings.TrimSpace(pendingHref.String()) == href
			}
			if inResponse && t.Name.Space == nsDAV && t.Name.Local == "response" && depth == responseDepth {
				if matched {
					out := make([]byte, 0, len(doc)+len(propsXML)+256)
					out = append(out, doc[:prevOffset]...)
					out = append(out, []byte(propstatBlock(propsXML))...)
					out = append(out, doc[prevOffset:]...)
					return out, true
				}
				inResponse = false
			}
			depth--
		}
		prevOffset = dec.InputOffset()
	}
}

// bufferedResponse captures a downstream handler's output so it can be
// rewritten before reaching the client.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) flushTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
