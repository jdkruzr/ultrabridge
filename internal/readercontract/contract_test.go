package readercontract

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jdkruzr/rhizome/server-go/registry"
)

func TestPinnedFingerprintAndProductionIsolation(t *testing.T) {
	got, err := Fingerprint(10000, 1000, nil)
	if err != nil || got != "08259be00cdab1bf6062941ec728a8c6b453a0ae2e4234ee71706d431062fef8" {
		t.Fatalf("empty fingerprint: %s %v", got, err)
	}
	p := make([]byte, 40)
	for i, x := range []uint32{100, 200} {
		binary.LittleEndian.PutUint32(p[i*20:], x)
		binary.LittleEndian.PutUint32(p[i*20+4:], 200)
		binary.LittleEndian.PutUint32(p[i*20+8:], 1000)
		binary.LittleEndian.PutUint32(p[i*20+16:], x)
	}
	ink := Ink{"ink", "ballpoint", -16777216, 10, 20, 1, 7, p, nil}
	got, err = Fingerprint(10000, 1000, []Ink{ink})
	if err != nil || got != "0460b7bc690c89dcdfd18560434dd62321358ad81d63c9b997336302c6c5efe4" {
		t.Fatalf("ink fingerprint: %s %v", got, err)
	}
	before := registry.ForestNote().SchemaHash()
	combined := CandidateCombined()
	if len(Registry().Tables) != 15 || len(combined.Tables) != len(registry.ForestNote().Tables)+15 || combined.SchemaHash() == before || registry.ForestNote().SchemaHash() != before {
		t.Fatal("candidate polluted production")
	}
	for _, table := range Registry().Tables {
		if table.Tombstone != "" || table.ServerAuthoredOnly {
			t.Fatal("domain state must not be generic tombstone/server-only", table.Name)
		}
	}
}

func TestUnicodeAndTrustBoundary(t *testing.T) {
	for _, raw := range []string{`"\ud800"`, `"\udc00"`, `"\ud800x"`, `"\ud800\ud800"`, "\"\xff\""} {
		if _, err := textJSON([]byte(raw)); err == nil {
			t.Fatalf("accepted lossy string %s", raw)
		}
	}
	for _, raw := range []string{`"\ud83d\udcda"`, `"\ufffd"`, `"literal \\ud800"`, `"📚<&>"`} {
		if _, err := textJSON([]byte(raw)); err != nil {
			t.Fatalf("rejected %s: %v", raw, err)
		}
	}
	if validID(strings.Repeat("📚", 257)) || !validID(strings.Repeat("📚", 256)) || validID("\u001c") || !validID("\u0085") {
		t.Fatal("UTF16 identity/blank mismatch")
	}
	if _, err := CompositeID("\xff"); err == nil {
		t.Fatal("accepted invalid UTF8 composite")
	}
	a := "0000000000000000000000000A"
	for _, verified := range []string{"", "account-name", "0000000000000000000000000B"} {
		if RequireClientAuthor(WireOp{SiteID: a}, verified) == nil {
			t.Fatal("unbound identity accepted")
		}
	}
	if RequireClientAuthor(WireOp{SiteID: a}, a) != nil {
		t.Fatal("verified identity rejected")
	}
}

func TestLosslessEnvelope(t *testing.T) {
	const valid = `{"table":"reader_book_title","pk":"book","site_id":"0000000000000000000000000A","op_ts":9223372036854775807,"op_seq":9007199254740993,"cols":{"title":"📚"}}`
	op, r, err := DecodeJSON([]byte(valid))
	if err != nil || op.OpTS != 9223372036854775807 || r.Version.OpSeq != 9007199254740993 {
		t.Fatalf("lost int64: %+v %v", op, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(valid), &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		copy := make(map[string]json.RawMessage)
		for k, v := range fields {
			copy[k] = v
		}
		delete(copy, key)
		raw, _ := json.Marshal(copy)
		if _, _, err := DecodeJSON(raw); err == nil {
			t.Fatalf("missing %s accepted", key)
		}
		copy[key] = json.RawMessage("null")
		raw, _ = json.Marshal(copy)
		if _, _, err := DecodeJSON(raw); err == nil {
			t.Fatalf("null %s accepted", key)
		}
	}
	for _, raw := range []string{
		strings.Replace(valid, `"pk":"book"`, `"pk":"\ud800"`, 1),
		strings.Replace(valid, `"title":"📚"`, `"title":"\udc00"`, 1),
		strings.Replace(valid, `9223372036854775807`, `9223372036854775808`, 1),
		strings.Replace(valid, `9223372036854775807`, `1.5`, 1),
		strings.Replace(valid, `"cols":`, `"TABLE":"reader_book","cols":`, 1),
	} {
		if _, _, err := DecodeJSON([]byte(raw)); err == nil {
			t.Fatalf("malformed envelope accepted: %s", raw)
		}
	}
}

type goldenVectors struct {
	Version                      int
	Registry                     registry.Registry
	Canonical, SchemaHash        string
	Production                   registry.Registry
	ProductionHash, CombinedHash string
	Wire                         []struct {
		Name          string
		Op            json.RawMessage
		Valid         bool
		Stored        json.RawMessage
		StoredVersion Version
	}
	Domain []struct {
		Name          string
		Op            json.RawMessage
		State, Reason string
		Existing      []struct {
			Op          json.RawMessage
			Unversioned bool
		}
	}
	Authors []struct {
		Op           json.RawMessage
		VerifiedSite string
		Valid        bool
	}
	Fingerprints []struct {
		Name          string
		Width, Height int64
		Hash          string
		Strokes       []json.RawMessage
		Bottoms       []int64
	}
	Composites []struct {
		Parts []string
		ID    string
	}
}

func normalizedRegistry(r registry.Registry) registry.Registry {
	r.Tables = append([]registry.Table(nil), r.Tables...)
	sort.Slice(r.Tables, func(i, j int) bool { return r.Tables[i].Name < r.Tables[j].Name })
	for i := range r.Tables {
		r.Tables[i].Columns = append([]registry.Column(nil), r.Tables[i].Columns...)
		cols := r.Tables[i].Columns
		sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	}
	return r
}

// The headless cross-repo runner first regenerates this file with the current
// Kotlin implementation, then requires this test to execute with that exact file.
// Standalone Go tests still run pinned, hostile-string and auth-boundary checks.
func TestKotlinContractVectors(t *testing.T) {
	path := os.Getenv("FORESTREAD_CONTRACT_VECTORS")
	if path == "" {
		t.Skip("run ForestNote docs/test-plans/forestread-stage-2/run.mjs for current Kotlin vectors")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v goldenVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v.Version != 1 || len(v.Wire) < 200 || len(v.Domain) < 28 || len(v.Fingerprints) < 23 || len(v.Authors) != 4 || len(v.Composites) < 8 {
		t.Fatal("missing contract coverage")
	}
	if !reflect.DeepEqual(normalizedRegistry(Registry()), normalizedRegistry(v.Registry)) {
		t.Fatal("typed registry differs (types, nullability, PK, tombstone or server-only flags)")
	}
	if Registry().Canonical() != v.Canonical || Registry().SchemaHash() != v.SchemaHash {
		t.Fatal("Rhizome schema hash/canonical mismatch")
	}
	if !reflect.DeepEqual(normalizedRegistry(registry.ForestNote()), normalizedRegistry(v.Production)) || registry.ForestNote().SchemaHash() != v.ProductionHash || CandidateCombined().SchemaHash() != v.CombinedHash {
		t.Fatal("actual writer registry or candidate combined schema differs")
	}
	for _, c := range v.Wire {
		t.Run("wire/"+c.Name, func(t *testing.T) {
			_, r, err := DecodeJSON(c.Op)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v, got %v", c.Valid, err)
			}
			// Compare actual typed values, including signed ARGB and byte-exact opaque JSON.
			if err == nil {
				gotJSON, err := json.Marshal(r.Columns)
				if err != nil {
					t.Fatal(err)
				}
				decode := func(data []byte) any {
					var value any
					d := json.NewDecoder(bytes.NewReader(data))
					d.UseNumber()
					if err := d.Decode(&value); err != nil {
						t.Fatal(err)
					}
					return value
				}
				if !reflect.DeepEqual(decode(gotJSON), decode(c.Stored)) || *r.Version != c.StoredVersion {
					t.Fatal("decoded values/provenance differ from Kotlin")
				}
			}
		})
	}
	for _, c := range v.Domain {
		t.Run("domain/"+c.Name, func(t *testing.T) {
			op, r, err := DecodeJSON(c.Op)
			if err != nil {
				t.Fatal(err)
			}
			type key struct{ table, id string }
			rows := map[key]*Record{}
			for _, e := range c.Existing {
				oldOp, old, err := DecodeJSON(e.Op)
				if err != nil {
					t.Fatal(err)
				}
				if e.Unversioned {
					old.Version = nil
				}
				rows[key{oldOp.Table, old.ID}] = &old
			}
			decision := Check(op.Table, r, func(table, id string) *Record { return rows[key{table, id}] })
			if decision != (Decision{c.State, c.Reason}) {
				t.Fatalf("got %+v, expected %s/%s", decision, c.State, c.Reason)
			}
		})
	}
	for i, c := range v.Authors {
		op, _, err := DecodeJSON(c.Op)
		if err != nil {
			t.Fatal(err)
		}
		if (RequireClientAuthor(op, c.VerifiedSite) == nil) != c.Valid {
			t.Fatalf("author vector %d differs", i)
		}
	}
	for _, c := range v.Fingerprints {
		t.Run("fingerprint/"+c.Name, func(t *testing.T) {
			var inks []Ink
			if len(c.Strokes) != len(c.Bottoms) {
				t.Fatal("missing ink bottoms")
			}
			for i, raw := range c.Strokes {
				_, r, err := DecodeJSON(raw)
				if err != nil {
					t.Fatal(err)
				}
				ink := r.ink()
				bottom, err := ink.Bottom()
				if err != nil || bottom != c.Bottoms[i] {
					t.Fatalf("bottom differs: %d %v", bottom, err)
				}
				inks = append(inks, ink)
			}
			hash, err := Fingerprint(c.Width, c.Height, inks)
			if err != nil || hash != c.Hash {
				t.Fatalf("fingerprint differs: %s %v", hash, err)
			}
		})
	}
	for _, c := range v.Composites {
		id, err := CompositeID(c.Parts...)
		if err != nil || id != c.ID {
			t.Fatalf("composite differs for %q: %s %v", c.Parts, id, err)
		}
	}
	t.Logf("Kotlin/Go parity: %d tables, %d wire, %d domain, %d author, %d fingerprint, %d composite vectors", len(v.Registry.Tables), len(v.Wire), len(v.Domain), len(v.Authors), len(v.Fingerprints), len(v.Composites))
}
