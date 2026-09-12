package readercontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math/rand"
	"os"
	"reflect"
	"sync"
	"testing"
)

func projectionFixture() AnnotationRows {
	v := &Version{10, 1, "0000000000000000000000000A"}
	return AnnotationRows{Annotation: Record{ID: "n", Version: v, Columns: map[string]any{
		"book_id":             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"initial_anchor_json": `{"version":1,"section":0,"start":0,"end":4,"quote":"text","prefix":"","suffix":""}`,
		"canvas_width":        int64(10000), "initial_height": int64(1000), "creator_session_id": "creator",
	}}, BookPresent: true, Sessions: []Record{{ID: "creator", Version: v, Columns: map[string]any{
		"annotation_id": "n", "kind": "interactive", "owner_site": v.SiteID, "state": "finished",
	}}}}
}

func TestReducePinnedHighlightAndImmutableParallelReads(t *testing.T) {
	rows := projectionFixture()
	before, _ := json.Marshal(rows)
	p := Reduce(rows)
	if p.Status != Ready || !p.Visible || p.HighlightPresent == nil || !*p.HighlightPresent || p.InputHash == nil || *p.InputHash != "08259be00cdab1bf6062941ec728a8c6b453a0ae2e4234ee71706d431062fef8" {
		t.Fatalf("bad highlight: %+v", p)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if got := Reduce(rows); !reflect.DeepEqual(p, got) {
					t.Error("parallel reduction differs")
				}
			}
		}()
	}
	wg.Wait()
	after, _ := json.Marshal(rows)
	if !bytes.Equal(before, after) {
		t.Fatal("reduction mutated snapshot")
	}
	rows.Sessions[0].Columns["state"] = "cancelled"
	if p := Reduce(rows); p.Status != Cancelled || p.Visible || p.InputHash != nil {
		t.Fatalf("bad cancellation %+v", p)
	}
	rows.BookDeleted = true
	if p := Reduce(rows); p.Status != Deleted || p.Visible || p.InputHash != nil {
		t.Fatal("delete must mask cancellation")
	}
}

func TestReduceFailsClosedOnMalformedSnapshot(t *testing.T) {
	for _, value := range []any{nil, "10000", int64(-1), float64(10000)} {
		rows := projectionFixture()
		rows.Annotation.Columns["canvas_width"] = value
		p := Reduce(rows)
		if p.Status != Invalid || p.Visible || p.InputHash != nil || p.EffectiveHeight != nil || len(p.Strokes) != 0 {
			t.Fatalf("did not fail closed: %+v", p)
		}
		if !reflect.DeepEqual(p.Issues, []ProjectionIssue{{"n", "Malformed stored annotation"}}) {
			t.Fatal("unstable malformed diagnostic")
		}
	}
}

func TestUTF16Ordering(t *testing.T) {
	for _, pair := range [][2]string{{"", "a"}, {"a", "aa"}, {"e\u0301", "é"}, {"📚", "\ue000"}, {"📚", "\uffff"}, {"📚", "📚x"}, {"\u0000", "a"}} {
		if compareUTF16(pair[0], pair[1]) >= 0 || compareUTF16(pair[1], pair[0]) <= 0 || compareUTF16(pair[0], pair[0]) != 0 {
			t.Fatalf("UTF16 order failed %q", pair)
		}
	}
}

// Snapshot fixtures use storage types (signed colors and preserved null provenance),
// not DecodeJSON: the reducer must also diagnose malformed/unsupported stored rows.
type snapshotRecord struct {
	ID      string
	Columns map[string]json.RawMessage
	Version *Version
}
type snapshotRows struct {
	Annotation                                  snapshotRecord
	BookPresent, BookDeleted, AnnotationDeleted bool
	Sessions, Strokes, Claims, Values           []snapshotRecord
}

func fixtureRecord(t *testing.T, r snapshotRecord) Record {
	t.Helper()
	result := Record{r.ID, map[string]any{}, r.Version}
	for key, raw := range r.Columns {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if n, ok := value.(json.Number); ok {
			var err error
			value, err = n.Int64()
			if err != nil {
				t.Fatal(err)
			}
		}
		if key == "points" || key == "point_dynamics" {
			if s, ok := value.(string); ok {
				b, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					t.Fatal(err)
				}
				value = b
			}
		}
		result.Columns[key] = value
	}
	return result
}
func fixtureRows(t *testing.T, r snapshotRows) AnnotationRows {
	group := func(records []snapshotRecord) []Record {
		result := make([]Record, 0, len(records))
		for _, r := range records {
			result = append(result, fixtureRecord(t, r))
		}
		return result
	}
	return AnnotationRows{fixtureRecord(t, r.Annotation), r.BookPresent, r.BookDeleted, r.AnnotationDeleted, group(r.Sessions), group(r.Strokes), group(r.Claims), group(r.Values)}
}
func jsonValue(t *testing.T, b []byte) any {
	t.Helper()
	var value any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestKotlinProjectionVectors(t *testing.T) {
	path := os.Getenv("FORESTREAD_PROJECTION_VECTORS")
	if path == "" {
		t.Skip("run ForestNote Stage 2 headless runner for fresh projection vectors")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Version, Permutations int
		Cases                 []struct {
			Name     string
			Rows     snapshotRows
			Expected json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Version != 1 || vectors.Permutations != 12 || len(vectors.Cases) < 60 {
		t.Fatal("incomplete projection vectors")
	}
	statuses := map[ProjectionStatus]bool{}
	for _, c := range vectors.Cases {
		t.Run(c.Name, func(t *testing.T) {
			rows := fixtureRows(t, c.Rows)
			before, _ := json.Marshal(rows)
			for seed := 0; seed < vectors.Permutations; seed++ {
				random := rand.New(rand.NewSource(int64(seed)))
				shuffle := func(records []Record) []Record {
					result := append([]Record{}, records...)
					random.Shuffle(len(result), func(i, j int) { result[i], result[j] = result[j], result[i] })
					return result
				}
				shuffled := rows
				shuffled.Sessions = shuffle(rows.Sessions)
				shuffled.Strokes = shuffle(rows.Strokes)
				shuffled.Claims = shuffle(rows.Claims)
				shuffled.Values = shuffle(rows.Values)
				p := Reduce(shuffled)
				statuses[p.Status] = true
				got, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(jsonValue(t, c.Expected), jsonValue(t, got)) {
					t.Fatalf("permutation %d differs\nexpected %s\nactual   %s", seed, c.Expected, got)
				}
			}
			after, _ := json.Marshal(rows)
			if !bytes.Equal(before, after) {
				t.Fatal("input snapshot mutated")
			}
		})
	}
	for _, status := range []ProjectionStatus{Ready, Deleted, Cancelled, Pending, Unsupported, Invalid} {
		if !statuses[status] {
			t.Errorf("missing status %s", status)
		}
	}
	t.Logf("Kotlin/Go full projection parity: %d cases x %d permutations", len(vectors.Cases), vectors.Permutations)
}
