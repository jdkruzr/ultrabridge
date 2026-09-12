package readersearch

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
)

type Alternative struct {
	Producer string `json:"producer"`
	Engine   string `json:"engine"`
	Model    any    `json:"model"`
	Language any    `json:"language"`
	Text     string `json:"text"`
}
type document struct {
	book, title, quote, text, anchor, hash, alternatives string
	revision                                             int64
}

func project(input *readerstore.SearchInput) (*document, error) {
	if input.Rows == nil {
		return nil, nil
	}
	p := readercontract.Reduce(*input.Rows)
	if p.Status != readercontract.Ready || !p.Visible || p.InputHash == nil || p.Anchor == nil {
		return nil, nil
	}
	d := &document{book: input.Rows.Annotation.Columns["book_id"].(string), anchor: *p.Anchor, hash: *p.InputHash, revision: input.Revision}
	if input.Title != nil {
		d.title = input.Title.Columns["title"].(string)
	} else if input.Book != nil {
		var metadata struct {
			Version int    `json:"version"`
			Title   string `json:"title"`
		}
		if json.Unmarshal([]byte(input.Book.Columns["metadata_json"].(string)), &metadata) == nil && metadata.Version == 1 {
			d.title = metadata.Title
		}
	}
	if p.HighlightPresent != nil && *p.HighlightPresent {
		var anchor struct {
			Quote string `json:"quote"`
		}
		if json.Unmarshal([]byte(d.anchor), &anchor) == nil {
			d.quote = anchor.Quote
		}
	}
	// Current alternatives only; preserve producer/model/language instead of
	// synthesizing a new OCR authority. Client class precedes future server class.
	matching := []readercontract.Record{}
	for _, r := range input.Recognition {
		if r.Columns["status"] == "ready" && r.Columns["input_hash"] == d.hash {
			matching = append(matching, r)
		}
	}
	sort.Slice(matching, func(i, j int) bool {
		a, b := matching[i], matching[j]
		ac := strings.HasPrefix(a.Columns["producer_id"].(string), "client:")
		bc := strings.HasPrefix(b.Columns["producer_id"].(string), "client:")
		if ac != bc {
			return ac
		}
		if a.Version.OpTS != b.Version.OpTS {
			return a.Version.OpTS > b.Version.OpTS
		}
		if a.Version.OpSeq != b.Version.OpSeq {
			return a.Version.OpSeq > b.Version.OpSeq
		}
		return a.Version.SiteID > b.Version.SiteID
	})
	alts := []Alternative{}
	seen := map[string]bool{}
	var texts []string
	for _, r := range matching {
		a := Alternative{r.Columns["producer_id"].(string), r.Columns["engine"].(string), r.Columns["model"], r.Columns["language"], r.Columns["text"].(string)}
		alts = append(alts, a)
		if !seen[a.Text] {
			seen[a.Text] = true
			texts = append(texts, a.Text)
		}
	}
	d.text = strings.Join(texts, "\n")
	b, err := json.Marshal(alts)
	if err != nil {
		return nil, err
	}
	d.alternatives = string(b)
	// Bound output as well as the source snapshot. Oversize never truncates and
	// never publishes partial/stale recognition as a successful index job.
	if len(d.title)+len(d.quote)+len(d.text)+len(d.anchor)+len(d.alternatives) > 256<<10 {
		return nil, readerstore.ErrBudget
	}
	return d, nil
}
