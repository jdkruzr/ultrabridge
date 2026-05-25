package dto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sysop/ultrabridge/internal/spcserver/envelope"
)

// TestSummaryDOWireKeys locks the SummaryDO JSON keys to the verbatim
// decompiled field names, including the casing traps (handwriteMD5 uppercase,
// isSummaryGroup/isDeleted as strings).
func TestSummaryDOWireKeys(t *testing.T) {
	b, _ := json.Marshal(SummaryDO{
		ID: 7, UniqueIdentifier: "u1", IsSummaryGroup: "N", IsDeleted: "N",
		MD5Hash: "m", HandwriteMD5: "h", CommentHandwriteName: "x.mark", SourceType: 2,
	})
	s := string(b)
	for _, want := range []string{
		`"id":7`, `"uniqueIdentifier":"u1"`, `"isSummaryGroup":"N"`, `"isDeleted":"N"`,
		`"md5Hash":"m"`, `"handwriteMD5":"h"`, `"commentHandwriteName":"x.mark"`, `"sourceType":2`,
		`"parentUniqueIdentifier":`, `"creationTime":`, `"lastModifiedTime":`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SummaryDO JSON missing %q: %s", want, s)
		}
	}
}

// TestSummaryInfoVOLowercaseHandwriteMd5 guards the one place the wire uses
// handwriteMd5 (lowercase d5), unlike handwriteMD5 everywhere else.
func TestSummaryInfoVOLowercaseHandwriteMd5(t *testing.T) {
	b, _ := json.Marshal(SummaryInfoVO{ID: 1, MD5Hash: "m", HandwriteMd5: "h", MetadataMap: map[string]string{"k": "v"}})
	s := string(b)
	if !strings.Contains(s, `"handwriteMd5":"h"`) {
		t.Errorf("SummaryInfoVO must use lowercase handwriteMd5: %s", s)
	}
	if strings.Contains(s, `"handwriteMD5"`) {
		t.Errorf("SummaryInfoVO must NOT use uppercase handwriteMD5: %s", s)
	}
	if !strings.Contains(s, `"metadataMap":{"k":"v"}`) {
		t.Errorf("SummaryInfoVO metadataMap shape wrong: %s", s)
	}
}

// TestQuerySummaryMD5HashVOFlat verifies the hash-query VO is flat with the
// summaryInfoVOList key and pagination fields.
func TestQuerySummaryMD5HashVOFlat(t *testing.T) {
	b, _ := json.Marshal(QuerySummaryMD5HashVO{
		BaseVO: envelope.OK(), TotalRecords: 1, TotalPages: 1, CurrentPage: 1, PageSize: 20,
		SummaryInfoVOList: []SummaryInfoVO{{ID: 1, MD5Hash: "m"}},
	})
	s := string(b)
	for _, want := range []string{`"success":true`, `"totalRecords":1`, `"summaryInfoVOList":[`, `"pageSize":20`} {
		if !strings.Contains(s, want) {
			t.Errorf("QuerySummaryMD5HashVO missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, `"data"`) {
		t.Errorf("must not nest under data: %s", s)
	}
}

// TestUploadSummaryApplyVOKeys locks the .mark upload-apply response keys.
func TestUploadSummaryApplyVOKeys(t *testing.T) {
	b, _ := json.Marshal(UploadSummaryApplyVO{
		BaseVO: envelope.OK(), FullUploadURL: "http://u/full", PartUploadURL: "http://u/part", InnerName: "abc.mark",
	})
	s := string(b)
	for _, want := range []string{`"fullUploadUrl":"http://u/full"`, `"partUploadUrl":"http://u/part"`, `"innerName":"abc.mark"`} {
		if !strings.Contains(s, want) {
			t.Errorf("UploadSummaryApplyVO missing %q: %s", want, s)
		}
	}
}

// TestAddSummaryDTODecode verifies request decoding tolerates the camelCase wire
// and the handwriteMD5 uppercase key.
func TestAddSummaryDTODecode(t *testing.T) {
	var d AddSummaryDTO
	body := `{"uniqueIdentifier":"u","fileId":12,"content":"c","sourceType":2,"md5Hash":"m","handwriteMD5":"h","creationTime":1000}`
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.UniqueIdentifier != "u" || d.FileID != 12 || d.Content != "c" || d.SourceType != 2 ||
		d.MD5Hash != "m" || d.HandwriteMD5 != "h" || d.CreationTime != 1000 {
		t.Fatalf("decoded wrong: %+v", d)
	}
}
