package caldav

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/sysop/ultrabridge/internal/fnpath"
	"github.com/sysop/ultrabridge/internal/taskattach"
	"github.com/sysop/ultrabridge/internal/taskstore"
)

// TaskToVTODO converts a task store Task to an ical.Calendar containing a VTODO.
// If the task has an ICalBlob, it deserializes the blob and overlays DB-authoritative
// fields on top. Otherwise, it builds the calendar from structured fields.
func TaskToVTODO(t *taskstore.Task, dueTimeMode string) *ical.Calendar {
	if t.ICalBlob.Valid && t.ICalBlob.String != "" {
		return taskToVTODOFromBlob(t, dueTimeMode)
	}
	return taskToVTODOFromFields(t, dueTimeMode)
}

// taskToVTODOFromFields builds a VTODO calendar from structured fields only.
// This is the original implementation, used for tasks without iCal blobs.
func taskToVTODOFromFields(t *taskstore.Task, dueTimeMode string) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//UltraBridge//CalDAV//EN")
	cal.Props.SetText("VERSION", "2.0")

	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", t.TaskID)

	// DTSTAMP is required by RFC 5545
	if t.LastModified.Valid {
		todo.Props.SetDateTime("DTSTAMP", taskstore.MsToTime(t.LastModified.Int64))
	} else {
		todo.Props.SetDateTime("DTSTAMP", time.Now().UTC())
	}

	if t.Title.Valid && t.Title.String != "" {
		todo.Props.SetText("SUMMARY", t.Title.String)
	}

	status := taskstore.CalDAVStatus(taskstore.NullStr(t.Status))
	todo.Props.SetText("STATUS", status)

	if t.DueTime != 0 {
		// Fields-only path: no blob to consult, so "preserve" falls
		// through to datetime (current behavior). Callers that need
		// floating-date semantics should round-trip through a blob.
		setDueOnTodo(todo, taskstore.MsToTime(t.DueTime), dueTimeMode)
	}

	if t.LastModified.Valid {
		lm := taskstore.MsToTime(t.LastModified.Int64)
		todo.Props.SetDateTime("LAST-MODIFIED", lm)
	}

	// Completion time: use last_modified (NOT completed_time) per Supernote quirk
	if ct, ok := taskstore.CompletionTime(t); ok {
		todo.Props.SetDateTime("COMPLETED", ct)
	}

	// Tier 2 fields
	if t.Detail.Valid && t.Detail.String != "" {
		todo.Props.SetText("DESCRIPTION", t.Detail.String)
	}
	if t.Importance.Valid && t.Importance.String != "" {
		todo.Props.SetText("PRIORITY", t.Importance.String)
	}

	// Links (read-only, informational)
	if t.Links.Valid && t.Links.String != "" {
		todo.Props.SetText("URL", t.Links.String)
	}

	cal.Children = append(cal.Children, todo)
	return cal
}

// taskToVTODOFromBlob deserializes the stored iCal blob and overlays
// DB-authoritative fields on top, preserving all Tier 3 properties.
func taskToVTODOFromBlob(t *taskstore.Task, dueTimeMode string) *ical.Calendar {
	dec := ical.NewDecoder(strings.NewReader(t.ICalBlob.String))
	cal, err := dec.Decode()
	if err != nil {
		// Fallback: if blob is corrupt, build from fields
		return taskToVTODOFromFields(t, dueTimeMode)
	}

	todo, err := FindVTODO(cal)
	if err != nil {
		return taskToVTODOFromFields(t, dueTimeMode)
	}

	// Overlay DB-authoritative fields (these may have been updated
	// via sync or direct DB operations since the blob was stored)
	todo.Props.SetText("UID", t.TaskID)

	if t.Title.Valid && t.Title.String != "" {
		todo.Props.SetText("SUMMARY", t.Title.String)
	}

	status := taskstore.CalDAVStatus(taskstore.NullStr(t.Status))
	todo.Props.SetText("STATUS", status)

	if t.DueTime != 0 {
		// "preserve" must honor the original VTODO's DUE form: a client
		// PUT of DUE;VALUE=DATE:YYYYMMDD (RFC 5545 floating all-day) must
		// be re-emitted as VALUE=DATE on the way back out. Promoting it
		// to UTC midnight datetime shifts the task to the previous
		// evening for clients in non-UTC timezones. The original prop is
		// still on the decoded blob at this point because we haven't
		// overlaid yet — read its ValueType before overwriting.
		setDueOnTodo(todo, taskstore.MsToTime(t.DueTime), dueTimeMode)
	} else {
		// Remove DUE if cleared
		delete(todo.Props, "DUE")
	}

	if t.LastModified.Valid {
		lm := taskstore.MsToTime(t.LastModified.Int64)
		todo.Props.SetDateTime("DTSTAMP", lm)
		todo.Props.SetDateTime("LAST-MODIFIED", lm)
	}

	if ct, ok := taskstore.CompletionTime(t); ok {
		todo.Props.SetDateTime("COMPLETED", ct)
	} else {
		delete(todo.Props, "COMPLETED")
	}

	// Overlay Tier 2 fields (may have been updated in DB after blob storage)
	if t.Detail.Valid && t.Detail.String != "" {
		todo.Props.SetText("DESCRIPTION", t.Detail.String)
	}
	if t.Importance.Valid && t.Importance.String != "" {
		todo.Props.SetText("PRIORITY", t.Importance.String)
	}

	return cal
}

// VTODOToTask extracts task fields from an ical.Calendar containing a VTODO.
// Returns the extracted task and the UID. Does not set user_id or task_id generation
// — caller handles those. Also serializes the full calendar as ICalBlob for round-trip fidelity.
func VTODOToTask(cal *ical.Calendar, dueTimeMode string) (*taskstore.Task, error) {
	var todo *ical.Component
	for _, child := range cal.Children {
		if child.Name == "VTODO" {
			todo = child
			break
		}
	}
	if todo == nil {
		return nil, fmt.Errorf("no VTODO component found")
	}

	t := &taskstore.Task{}

	if uid := todo.Props.Get("UID"); uid != nil {
		t.TaskID = uid.Value
	}
	if summary := todo.Props.Get("SUMMARY"); summary != nil {
		// .Text() un-escapes per RFC 5545 (\\ → \, \n → real newline,
		// \, → comma, \; → semicolon). Reading .Value raw preserves the
		// backslash escapes, which compounded across PUT/pull cycles
		// (each round doubled backslashes and turned newlines into
		// literal "\n"). Falling back to .Value on parse error keeps
		// behavior safe for malformed input.
		if v, err := summary.Text(); err == nil {
			t.Title = taskstore.SqlStr(v)
		} else {
			t.Title = taskstore.SqlStr(summary.Value)
		}
	}
	if status := todo.Props.Get("STATUS"); status != nil {
		t.Status = taskstore.SqlStr(taskstore.SupernoteStatus(status.Value))
	}
	if due := todo.Props.Get("DUE"); due != nil {
		dueTime, err := due.DateTime(time.UTC)
		if err == nil {
			if dueTimeMode == "date_only" {
				// Strip time component
				dueTime = time.Date(dueTime.Year(), dueTime.Month(), dueTime.Day(),
					0, 0, 0, 0, time.UTC)
			}
			t.DueTime = taskstore.TimeToMs(dueTime)
		}
	}
	if desc := todo.Props.Get("DESCRIPTION"); desc != nil {
		// Same RFC 5545 un-escape rationale as SUMMARY above.
		if v, err := desc.Text(); err == nil {
			t.Detail = taskstore.SqlStr(v)
		} else {
			t.Detail = taskstore.SqlStr(desc.Value)
		}
	}
	if prio := todo.Props.Get("PRIORITY"); prio != nil {
		t.Importance = taskstore.SqlStr(prio.Value)
	}
	if u := todo.Props.Get("URL"); u != nil {
		// URL is a URI value; FN emits the https deep link back to the source
		// page here (paired with the X-FORESTNOTE-NATIVE-URL forestnote:// form).
		// Lift it into the links column — TaskToVTODO emits URL from t.Links, so
		// without this the inbound link round-trips only inside the blob and never
		// surfaces in REST/MCP. Mirror SUMMARY/DESCRIPTION: prefer .Text() (un-escapes
		// any SetText-escaped round-trip), fall back to raw .Value; empty → leave NULL.
		v, err := u.Text()
		if err != nil {
			v = u.Value
		}
		if v != "" {
			t.Links = taskstore.SqlStr(v)
		}
	}

	// Handle completion time mapping (Supernote quirk: last_modified = actual completion time)
	if taskstore.NullStr(t.Status) == "completed" {
		now := time.Now().UnixMilli()
		if completed := todo.Props.Get("COMPLETED"); completed != nil {
			ct, err := completed.DateTime(time.UTC)
			if err == nil {
				t.LastModified = sql.NullInt64{Int64: taskstore.TimeToMs(ct), Valid: true}
			} else {
				t.LastModified = sql.NullInt64{Int64: now, Valid: true}
			}
		} else {
			t.LastModified = sql.NullInt64{Int64: now, Valid: true}
		}
	}

	// Extract ForestNote provenance (X-FORESTNOTE-*) into structured columns so
	// the REST/MCP filter surface doesn't have to re-parse the blob on every read.
	// The blob still carries the raw bytes either way.
	extractForestNoteMetadata(todo, t)

	// Store full VCALENDAR as blob for round-trip fidelity
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err == nil {
		t.ICalBlob = sql.NullString{String: buf.String(), Valid: true}
	} else {
		slog.Warn("failed to encode ical blob", "err", err)
	}

	return t, nil
}

// setDueOnTodo writes the DUE property in the form dictated by dueTimeMode:
//
//   - "date_only": always emit DUE;VALUE=DATE:YYYYMMDD
//   - "datetime":  always emit DUE:YYYYMMDDTHHMMSSZ
//   - "preserve" (and anything else, including the default empty string):
//     keep whatever VALUE the existing DUE prop on this VTODO carries — DATE
//     stays DATE, DATE-TIME stays DATE-TIME, and if there is no existing DUE
//     prop (e.g. fields-only synth) we fall back to DATE-TIME.
//
// The caller is responsible for having already merged any blob into todo; we
// only inspect ValueType, never values, so passing a freshly-built todo with
// no DUE is safe.
func setDueOnTodo(todo *ical.Component, due time.Time, dueTimeMode string) {
	switch dueTimeMode {
	case "date_only":
		todo.Props.SetDate("DUE", due)
		return
	case "datetime":
		todo.Props.SetDateTime("DUE", due)
		return
	}
	// preserve (or unset / unknown)
	if existing := todo.Props.Get("DUE"); existing != nil && existing.ValueType() == ical.ValueDate {
		todo.Props.SetDate("DUE", due)
		return
	}
	todo.Props.SetDateTime("DUE", due)
}

// FindVTODO returns the first VTODO component in the calendar, or error.
func FindVTODO(cal *ical.Calendar) (*ical.Component, error) {
	for _, child := range cal.Children {
		if child.Name == "VTODO" {
			return child, nil
		}
	}
	return nil, fmt.Errorf("no VTODO component found")
}

// HasVEvent returns true if the calendar contains a VEVENT component.
func HasVEvent(cal *ical.Calendar) bool {
	for _, child := range cal.Children {
		if child.Name == "VEVENT" {
			return true
		}
	}
	return false
}

// BlobMetadata is the subset of VTODO properties we read out of the stored
// ical_blob at response-mapping time (instead of at PUT time). Categories,
// the FN native URL, and COMMENT stay blob-only — the categories list because
// it's list-shaped and we'd rather not normalize it into a column; the native
// URL because it lives in X-FORESTNOTE-NATIVE-URL (an extension property
// paired with the structured URL); COMMENT because it can be arbitrary-size
// text (FN may stuff the full recognized text here) and pinning a TEXT column
// for it isn't worth the schema churn yet.
type BlobMetadata struct {
	Categories []string
	NativeURL  string
	Comment    string
	// Attachments surfaces VTODO ATTACH properties (RFC 5545 §3.8.1.1) in
	// document order. Inline-binary attachments carry metadata only (never the
	// base64 payload) so the task JSON stays small.
	Attachments []BlobAttachment
}

// BlobAttachment is one ATTACH property surfaced from the stored blob. It
// carries metadata only — never the inline bytes — so a multi-megabyte
// embedded attachment doesn't bloat the task JSON. For a URI attachment, URI
// holds the link. For inline binary, URI is empty until UB serves the bytes
// via a signed endpoint (wired with the side-store/de-bloat path); Size is the
// decoded byte length and Inline is true.
type BlobAttachment struct {
	URI      string // ATTACH value when it is a URI; "" for not-yet-served inline binary
	FmtType  string // FMTTYPE param (MIME type), if present
	Filename string // FILENAME / X-FILENAME param, if present
	Size     int64  // decoded byte length for inline binary; 0 for URI
	Inline   bool   // true when the source ATTACH was inline BASE64 binary
}

// ParseBlobMetadata extracts category and native-URL info from a stored
// VCALENDAR blob. Returns a zero-value BlobMetadata on any parse failure
// (blank/corrupt blob, no VTODO, etc.) — never errors. Callers should treat
// an empty Categories list as "no categories" rather than as a parse error.
func ParseBlobMetadata(blob string) BlobMetadata {
	if blob == "" {
		return BlobMetadata{}
	}
	cal, err := ical.NewDecoder(strings.NewReader(blob)).Decode()
	if err != nil {
		return BlobMetadata{}
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		return BlobMetadata{}
	}
	out := BlobMetadata{}
	// CATEGORIES: RFC 5545 §3.8.1.2 — multi-valued TEXT list. The COMMA inside
	// a CATEGORIES value is the list separator (NOT a single-value content
	// comma; that would be escaped as `\,` per §3.3.11). go-ical's TextList()
	// does the unescape-and-split correctly; a plain Text() / Value here would
	// truncate at the first comma. CATEGORIES may also appear multiple times;
	// we coalesce all occurrences into one slice.
	for _, p := range todo.Props.Values("CATEGORIES") {
		items, terr := p.TextList()
		if terr != nil {
			// Fall back to raw .Value for badly-typed properties (rare).
			items = strings.Split(p.Value, ",")
		}
		for _, c := range items {
			c = strings.TrimSpace(c)
			if c != "" {
				out.Categories = append(out.Categories, c)
			}
		}
	}
	// X-FORESTNOTE-NATIVE-URL: blob-only (sibling of the structured URL).
	if p := todo.Props.Get("X-FORESTNOTE-NATIVE-URL"); p != nil {
		if t, terr := p.Text(); terr == nil && t != "" {
			out.NativeURL = t
		} else if p.Value != "" {
			out.NativeURL = p.Value
		}
	}
	// COMMENT: RFC 5545 §3.8.1.4 — TEXT property, may appear multiple times;
	// we join occurrences with a blank line so multi-COMMENT VTODOs round-trip
	// to a readable single string. Most clients (incl. FN) emit one.
	for _, p := range todo.Props.Values("COMMENT") {
		c, terr := p.Text()
		if terr != nil {
			c = p.Value
		}
		if c == "" {
			continue
		}
		if out.Comment != "" {
			out.Comment += "\n\n"
		}
		out.Comment += c
	}
	// ATTACH: RFC 5545 §3.8.1.1 — either a URI (the default value type) or an
	// inline BASE64 binary (ENCODING=BASE64;VALUE=BINARY). May repeat; we keep
	// every occurrence in document order. For inline binary we surface metadata
	// (size/fmttype/filename) but NOT the base64 payload — dumping megabytes of
	// it into the task JSON would defeat the purpose. The signed fetch URL for
	// inline attachments is filled in once the side-store/de-bloat path exists.
	for _, p := range todo.Props.Values(ical.PropAttach) {
		a := BlobAttachment{
			FmtType:  p.Params.Get("FMTTYPE"),
			Filename: attachFilename(&p),
		}
		switch {
		case p.Params.Get("X-UB-INLINE") == "1":
			// De-bloated inline binary: the value is the signed fetch URL and
			// the decoded size is recorded in X-UB-SIZE (the bytes live in the
			// content store, not the blob).
			a.Inline = true
			a.URI = p.Value
			if n, perr := strconv.ParseInt(p.Params.Get("X-UB-SIZE"), 10, 64); perr == nil {
				a.Size = n
			}
		case isInlineBinaryAttach(&p):
			// Raw inline binary still in the blob (no-deps / pre-de-bloat path):
			// surface metadata only, never the base64 payload.
			a.Inline = true
			a.Size = base64DecodedSize(p.Value)
		default:
			a.URI = p.Value
		}
		out.Attachments = append(out.Attachments, a)
	}
	return out
}

// isInlineBinaryAttach reports whether an ATTACH property carries inline
// BASE64 binary content rather than a URI. RFC 5545 §3.8.1.1 signals inline
// binary with VALUE=BINARY (always paired with ENCODING=BASE64); we accept
// either marker defensively since some emitters set only one.
func isInlineBinaryAttach(p *ical.Prop) bool {
	return p.ValueType() == ical.ValueBinary ||
		strings.EqualFold(p.Params.Get(ical.ParamEncoding), "BASE64")
}

// base64DecodedSize returns the decoded byte length of a standard-base64 string
// without allocating the decoded bytes — cheap even for a multi-MB inline
// attachment. Falls back to a real decode for malformed (non-multiple-of-4)
// input, returning 0 if that also fails.
func base64DecodedSize(s string) int64 {
	s = strings.TrimSpace(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%4 != 0 {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return int64(len(b))
		}
		return 0
	}
	pad := 0
	if strings.HasSuffix(s, "==") {
		pad = 2
	} else if strings.HasSuffix(s, "=") {
		pad = 1
	}
	return int64(n/4*3 - pad)
}

// attachFilename returns the human filename hint on an ATTACH property, if any.
// FILENAME is non-standard but widely emitted (Apple Calendar, and Thunderbird
// via X-FILENAME); we check the plain form first, then the X- vendor form.
func attachFilename(p *ical.Prop) string {
	if v := p.Params.Get("FILENAME"); v != "" {
		return v
	}
	return p.Params.Get("X-FILENAME")
}

// AttachDeps carries the side-store, signer, and external base URL the CalDAV
// layer uses to de-bloat inline-binary ATTACH on inbound and reconstruct it on
// outbound. A zero value (nil Store/Signer) disables the behavior — inline
// binary then simply stays in the ical_blob, exactly as before.
type AttachDeps struct {
	Store   *taskattach.BlobStore
	Signer  *taskattach.Signer
	BaseURL string // external origin for absolute fetch URLs; "" → relative path
}

func (d AttachDeps) enabled() bool { return d.Store != nil && d.Signer != nil }

// downloadURL builds the (absolute when BaseURL is set, else relative) signed
// fetch URL for de-bloated content, appending cosmetic type/name params the
// download handler uses for Content-Type / Content-Disposition.
func (d AttachDeps) downloadURL(sha, fmttype, filename string) string {
	u := d.Signer.SignedAttachmentPath(sha)
	if fmttype != "" {
		u += "&type=" + url.QueryEscape(fmttype)
	}
	if filename != "" {
		u += "&name=" + url.QueryEscape(filename)
	}
	if d.BaseURL != "" {
		return strings.TrimRight(d.BaseURL, "/") + u
	}
	return u
}

// DebloatInlineAttachments rewrites every inline-binary ATTACH in the given
// VCALENDAR blob into a compact URI reference: the bytes are written to the
// content store (deduped by sha256) and the property becomes a signed download
// URL carrying X-UB-INLINE / X-UB-SHA256 / X-UB-ENC / X-UB-SIZE markers plus
// the original FMTTYPE / FILENAME. This keeps the stored ical_blob (a TEXT
// column re-encoded on every round-trip) small even when a client embeds
// megabytes. The bytes are fully recoverable via ReconstructInlineAttachments,
// so the round-trip stays lossless.
//
// Crucially, ComputeETag ignores the blob, so this rewrite does NOT change the
// CalDAV ETag and never triggers a spurious re-sync.
//
// Defensive throughout: a blank/corrupt blob, an undecodable base64 value, or a
// store-write error leaves that property (or the whole blob) untouched rather
// than erroring — the worst case degrades to "inline binary stayed in the blob".
func DebloatInlineAttachments(blob string, deps AttachDeps) string {
	if blob == "" || !deps.enabled() {
		return blob
	}
	cal, err := ical.NewDecoder(strings.NewReader(blob)).Decode()
	if err != nil {
		return blob
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		return blob
	}
	props := todo.Props[ical.PropAttach]
	changed := false
	for i := range props {
		p := &props[i]
		if p.Params.Get("X-UB-INLINE") == "1" {
			continue // already de-bloated (idempotent)
		}
		if !isInlineBinaryAttach(p) {
			continue // URI attachment — leave alone
		}
		raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(p.Value))
		if derr != nil {
			continue
		}
		sha, size, perr := deps.Store.Put(raw)
		if perr != nil {
			continue
		}
		fmttype := p.Params.Get("FMTTYPE")
		filename := attachFilename(p)
		p.Value = deps.downloadURL(sha, fmttype, filename)
		p.Params.Del(ical.ParamEncoding)
		p.Params.Del(ical.ParamValue)
		p.Params.Set("X-UB-INLINE", "1")
		p.Params.Set("X-UB-SHA256", sha)
		p.Params.Set("X-UB-ENC", "BASE64")
		p.Params.Set("X-UB-SIZE", strconv.FormatInt(size, 10))
		changed = true
	}
	if !changed {
		return blob
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return blob // keep the original rather than emit garbage
	}
	return buf.String()
}

// ReconstructInlineAttachments restores inline-binary ATTACH properties that
// DebloatInlineAttachments compacted: it reads the bytes back from the content
// store and re-embeds them as ENCODING=BASE64;VALUE=BINARY (via Prop.SetBinary)
// so the consuming CalDAV client receives byte-equivalent content, then drops
// the X-UB-* markers. Reconstruction is decode-equivalent, not byte-identical:
// go-ical re-serializes param maps in unspecified order (every PUT already does
// this). A missing store file leaves the property as its still-fetchable URI
// form rather than failing the whole response.
func ReconstructInlineAttachments(cal *ical.Calendar, store *taskattach.BlobStore) {
	if cal == nil || store == nil {
		return
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		return
	}
	props := todo.Props[ical.PropAttach]
	for i := range props {
		p := &props[i]
		if p.Params.Get("X-UB-INLINE") != "1" {
			continue
		}
		f, _, oerr := store.Open(p.Params.Get("X-UB-SHA256"))
		if oerr != nil {
			continue // leave the URI form intact
		}
		raw, rerr := io.ReadAll(f)
		f.Close()
		if rerr != nil {
			continue
		}
		p.SetBinary(raw) // sets VALUE=BINARY + ENCODING=BASE64 + base64 value
		p.Params.Del("X-UB-INLINE")
		p.Params.Del("X-UB-SHA256")
		p.Params.Del("X-UB-ENC")
		p.Params.Del("X-UB-SIZE")
	}
}

// AddFNRenderAttach appends an ATTACH pointing at the signed public render of
// the task's source ForestNote page, so a third-party CalDAV client shows the
// handwriting page as a tappable attachment. It is SYNTHESIZED at serve time
// only — never written to the stored blob — so it causes no blob churn and
// never interacts with de-bloat. Requires a signer + an external base URL
// (absolute URLs are mandatory for third-party clients) and both FN notebook +
// page id columns. Idempotent: never emits a second FN-render ATTACH.
func AddFNRenderAttach(cal *ical.Calendar, t *taskstore.Task, deps AttachDeps) {
	if cal == nil || deps.Signer == nil || deps.BaseURL == "" {
		return
	}
	if !t.ForestNoteNotebookID.Valid || t.ForestNoteNotebookID.String == "" ||
		!t.ForestNotePageID.Valid || t.ForestNotePageID.String == "" {
		return
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		return
	}
	for _, p := range todo.Props.Values(ical.PropAttach) {
		if p.Params.Get("X-UB-FN-RENDER") == "1" {
			return // already present — emit at most one
		}
	}
	notePath := fnpath.Page(t.ForestNoteNotebookID.String, t.ForestNotePageID.String)
	att := ical.NewProp(ical.PropAttach)
	att.Value = strings.TrimRight(deps.BaseURL, "/") + deps.Signer.SignedFNRenderPath(notePath)
	att.Params.Set("FMTTYPE", "image/jpeg")
	att.Params.Set("X-UB-FN-RENDER", "1")
	todo.Props[ical.PropAttach] = append(todo.Props[ical.PropAttach], *att)
}

// BuildBlobWithMetadata constructs a minimal VCALENDAR blob carrying only
// the metadata properties that have no structured column (CATEGORIES,
// COMMENT, X-FORESTNOTE-NATIVE-URL). The blob carries UID + DTSTAMP only;
// SUMMARY is deliberately omitted so the blob-overlay path (taskToVTODOFromBlob)
// can inject the live Title column at serve time — emitting an empty
// `SUMMARY:` here would round-trip through strict CalDAV clients as a
// malformed VTODO (RFC 5545 §3.6.2 disallows empty TEXT SUMMARY).
//
// Returns "" when meta carries no payload — callers should leave ICalBlob
// NULL in that case rather than store an empty-marker blob.
func BuildBlobWithMetadata(taskID string, meta BlobMetadata) string {
	if len(meta.Categories) == 0 && meta.Comment == "" && meta.NativeURL == "" {
		return ""
	}
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//UltraBridge//CalDAV//EN")
	cal.Props.SetText("VERSION", "2.0")

	todo := ical.NewComponent("VTODO")
	todo.Props.SetText("UID", taskID)
	todo.Props.SetDateTime("DTSTAMP", time.Now().UTC())

	if len(meta.Categories) > 0 {
		setCategoriesProp(todo, meta.Categories)
	}
	if meta.Comment != "" {
		todo.Props.SetText("COMMENT", meta.Comment)
	}
	if meta.NativeURL != "" {
		todo.Props.SetText("X-FORESTNOTE-NATIVE-URL", meta.NativeURL)
	}

	cal.Children = append(cal.Children, todo)

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		// Encoding shouldn't fail for a hand-built calendar, but if it does we
		// drop the blob rather than persist garbage. Caller sees "" and skips.
		return ""
	}
	return buf.String()
}

// BlobMetadataPatch is the patch-shaped sibling of BlobMetadata used by
// MergeBlobMetadataPatch. Each field is a pointer so the merge can
// distinguish "leave the existing value alone" (nil) from "replace with
// this, even if empty" (non-nil). This matters because the parse → merge
// → re-serialize cycle on Update would otherwise lose CATEGORIES or COMMENT
// when a partial-corrupt blob slips through ParseBlobMetadata's defensive
// zero-value fallback: a "leave alone" semantic that's only expressible
// at the patch boundary prevents the silent clear.
//
// CategoriesPtr semantics: nil = unchanged, non-nil (incl. empty slice) =
// replace wholesale. Matches the *[]string contract on service.TaskPatch.
type BlobMetadataPatch struct {
	CategoriesPtr *[]string
	CommentPtr    *string
	// ClearComment forces COMMENT removal even when CommentPtr is nil.
	// Maps the Clear* sentinel pattern used at the service/REST layer.
	ClearComment bool
	NativeURLPtr *string
}

// MergeBlobMetadataPatch takes an existing blob and applies only the fields
// the caller explicitly asked to touch — preserving CATEGORIES, COMMENT, and
// X-FORESTNOTE-NATIVE-URL when the patch leaves them at nil, regardless of
// what the parsed blob's metadata says. All other properties on the VTODO
// (X-FORESTNOTE-NOTEBOOK-ID, PRIORITY, VALARM children, etc.) survive
// untouched.
//
// On a corrupt/empty existing blob, falls back to BuildBlobWithMetadata with
// the patch's non-nil fields synthesized into a fresh BlobMetadata — a
// partial blob loss can't silently revive cleared categories, since the
// caller never asked for them in the first place.
func MergeBlobMetadataPatch(taskID, existingBlob string, patch BlobMetadataPatch) string {
	fresh := BlobMetadata{}
	if patch.CategoriesPtr != nil {
		fresh.Categories = *patch.CategoriesPtr
	}
	if patch.CommentPtr != nil && !patch.ClearComment {
		fresh.Comment = *patch.CommentPtr
	}
	if patch.NativeURLPtr != nil {
		fresh.NativeURL = *patch.NativeURLPtr
	}

	if existingBlob == "" {
		return BuildBlobWithMetadata(taskID, fresh)
	}
	cal, err := ical.NewDecoder(strings.NewReader(existingBlob)).Decode()
	if err != nil {
		return BuildBlobWithMetadata(taskID, fresh)
	}
	todo, err := FindVTODO(cal)
	if err != nil || todo == nil {
		return BuildBlobWithMetadata(taskID, fresh)
	}

	// CATEGORIES: only touch when the caller asked. Replace wholesale on a
	// non-nil patch (matching the service-layer *[]string contract);
	// preserve the existing values when nil.
	if patch.CategoriesPtr != nil {
		todo.Props.Del("CATEGORIES")
		if len(*patch.CategoriesPtr) > 0 {
			setCategoriesProp(todo, *patch.CategoriesPtr)
		}
	}

	// COMMENT: ClearComment always wins; otherwise touch only when CommentPtr
	// is non-nil. Empty *CommentPtr clears as well — RFC 5545 disallows empty
	// COMMENT TEXT so empty-string is equivalent to absent.
	switch {
	case patch.ClearComment:
		todo.Props.Del("COMMENT")
	case patch.CommentPtr != nil:
		todo.Props.Del("COMMENT")
		if *patch.CommentPtr != "" {
			todo.Props.SetText("COMMENT", *patch.CommentPtr)
		}
	}

	// X-FORESTNOTE-NATIVE-URL: same pattern as COMMENT. No write path in the
	// service layer offers this today (NativeURL is FN-emitted via CalDAV
	// PUT only), but the merge contract is now symmetric — a future "clear"
	// flag wouldn't need to revisit this code.
	if patch.NativeURLPtr != nil {
		todo.Props.Del("X-FORESTNOTE-NATIVE-URL")
		if *patch.NativeURLPtr != "" {
			todo.Props.SetText("X-FORESTNOTE-NATIVE-URL", *patch.NativeURLPtr)
		}
	}

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return existingBlob // pessimistic: keep old blob on encoder failure
	}
	return buf.String()
}

// setCategoriesProp installs (or replaces) a CATEGORIES multi-value property
// on the given VTODO, escaping each list item per RFC 5545 §3.3.11 before
// the comma-join (so a category value containing a literal comma round-trips
// as `\,` instead of being treated as a list separator). No-op when cats is
// empty after filtering blanks; callers Del the existing prop first if a
// total clear is intended.
func setCategoriesProp(todo *ical.Component, cats []string) {
	parts := make([]string, 0, len(cats))
	for _, c := range cats {
		if c == "" {
			continue
		}
		parts = append(parts, escapeICalText(c))
	}
	if len(parts) == 0 {
		return
	}
	p := ical.NewProp("CATEGORIES")
	p.Value = strings.Join(parts, ",")
	todo.Props.Set(p)
}

// escapeICalText applies RFC 5545 §3.3.11 TEXT escaping (backslash, comma,
// semicolon, newline). Used inside multi-value CATEGORIES where go-ical's
// SetText helper would escape the list-separator commas as well.
func escapeICalText(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, ",", `\,`)
	s = strings.ReplaceAll(s, ";", `\;`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// extractForestNoteMetadata reads any X-FORESTNOTE-* properties off the VTODO
// and stamps the matching ForestNote* fields on the task. The blob path keeps
// the bytes intact regardless; this helper is purely about lifting the values
// into structured columns so the REST API and MCP can filter on them.
//
// Defensive: every read tolerates the property being absent (the common case for
// non-ForestNote clients). Empty strings are stored as NULL (sql.NullString
// zero-value) so a sender that emits the property with no value matches the
// SQL filter "WHERE forestnote_notebook_id IS NOT NULL" exactly.
func extractForestNoteMetadata(todo *ical.Component, t *taskstore.Task) {
	t.ForestNoteNotebookID = nullStringFromProp(todo, "X-FORESTNOTE-NOTEBOOK-ID")
	t.ForestNotePageID = nullStringFromProp(todo, "X-FORESTNOTE-PAGE-ID")
	t.ForestNoteNotebookName = nullStringFromProp(todo, "X-FORESTNOTE-NOTEBOOK-NAME")
	t.ForestNoteSource = nullStringFromProp(todo, "X-FORESTNOTE-SOURCE")
}

// nullStringFromProp returns the named property's value as a sql.NullString,
// using `.Text()` so RFC 5545 TEXT escapes (`\,` `\;` `\n` `\\`) are unescaped
// for properties of TEXT type; falls back to the raw Value for non-TEXT props
// (where go-ical leaves Text() returning an error).
func nullStringFromProp(c *ical.Component, name string) sql.NullString {
	p := c.Props.Get(name)
	if p == nil {
		return sql.NullString{}
	}
	if v, err := p.Text(); err == nil && v != "" {
		return sql.NullString{String: v, Valid: true}
	}
	if p.Value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: p.Value, Valid: true}
}
