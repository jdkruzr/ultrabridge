package syncstore

import (
	"encoding/json"
	"sort"

	"github.com/jdkruzr/rhizome/server-go/bounded"
)

// ExchangeBounded is atomic even when the response is too large. Only the new
// log suffix/seen entries and this site's acknowledgement need undoing; never
// clone the entire relay backlog just to construct a bounded page.
func (s *Store) ExchangeBounded(site string, cursor int64, ops []Op, limits bounded.Limits) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	oldLen, oldSeq := len(s.log), s.seq
	oldAck, hadAck := s.acked[site]
	committed := false
	defer func() {
		if !committed {
			for _, rec := range s.log[oldLen:] {
				delete(s.seen, opID{rec.op.SiteID, rec.op.OpSeq})
			}
			clear(s.log[oldLen:])
			s.log = s.log[:oldLen]
			s.seq = oldSeq
			if hadAck {
				s.acked[site] = oldAck
			} else {
				delete(s.acked, site)
			}
		}
	}()
	res := s.applyBatch(site, ops)
	rejected, err := json.Marshal(res.Rejected)
	if err != nil {
		return nil, err
	}
	page, err := bounded.NewPage(cursor, res.AcceptedThrough, rejected, limits)
	if err != nil {
		return nil, err
	}
	start := sort.Search(len(s.log), func(i int) bool { return s.log[i].seq > cursor })
	for _, rec := range s.log[start:] {
		if rec.op.SiteID == site {
			continue
		}
		if page.CountFull() {
			page.Response.HasMore = true
			break
		}
		raw, err := json.Marshal(rec.op)
		if err != nil {
			return nil, err
		}
		added, err := page.Add(rec.seq, rec.op.SiteID, rec.op.OpSeq, int64(len(raw)), raw)
		if err != nil {
			return nil, err
		}
		if !added {
			break
		}
	}
	body, err := page.Encode()
	if err != nil {
		return nil, err
	}
	committed = true
	return body, nil
}
