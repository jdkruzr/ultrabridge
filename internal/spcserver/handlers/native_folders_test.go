package handlers

import (
	"context"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Replays the Nomad's observed create-folder(local path) → upload(native path)
// sequence. The folder ID recorded by the device must still be the only folder
// identity reported after the upload; two IDs caused a live Document conflict.
func TestLocalCreateThenNativeUploadKeepsFolderIdentity(t *testing.T) {
	for _, tc := range []struct{ local, native string }{{"Document", "DOCUMENT/Document"}, {"Note", "NOTE/Note"}, {"MyStyle", "NOTE/MyStyle"}} {
		t.Run(tc.local, func(t *testing.T) {
			root := t.TempDir()
			up := newUploadHandler(t, root)
			files := &FileHandler{Root: root, Reg: up.Reg}
			created := decodeMap(t, files.CreateFolderV2, fmt.Sprintf(`{"path":%q,"autorename":false}`, "/"+tc.local))
			if created["success"] != true {
				t.Fatal(created)
			}
			metadata := created["metadata"].(map[string]any)
			folderID := metadata["id"].(string)
			if metadata["path_display"] != "/"+tc.native {
				t.Fatalf("wrong native path: %v", metadata)
			}
			for i, parent := range []string{"/" + tc.native, "/" + tc.local} {
				filename := fmt.Sprintf("book%d.epub", i)
				payload := "synthetic epub bytes"
				apply := decodeMap(t, up.Apply, fmt.Sprintf(`{"path":%q,"fileName":%q,"size":%q}`, parent+"/"+filename, filename, strconv.Itoa(len(payload))))
				if apply["success"] != true {
					t.Fatal(apply)
				}
				inner := apply["innerName"].(string)
				if _, err := up.Staging.Stage(inner, strings.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
				finish := decodeMap(t, up.Finish, fmt.Sprintf(`{"path":%q,"fileName":%q,"size":%q,"content_hash":%q,"innerName":%q}`, parent, filename, strconv.Itoa(len(payload)), fmt.Sprintf("%x", md5.Sum([]byte(payload))), inner))
				if finish["success"] != true || finish["path_display"] != "/"+tc.native+"/"+filename {
					t.Fatalf("finish: %v", finish)
				}
			}
			gotID, err := up.Reg.IDFor(context.Background(), filepath.Join(root, tc.native))
			if err != nil || strconv.FormatInt(gotID, 10) != folderID {
				t.Fatalf("folder identity changed: %d %s %v", gotID, folderID, err)
			}
			if _, err := os.Stat(filepath.Join(root, tc.local)); !os.IsNotExist(err) {
				t.Fatalf("duplicate local folder created: %v", err)
			}
			listing := decodeMap(t, files.ListFolder, `{"id":"0","recursive":true}`)
			count := 0
			for _, raw := range listing["entries"].([]any) {
				e := raw.(map[string]any)
				if e["path_display"] == "/"+tc.native {
					count++
					if e["id"] != folderID {
						t.Fatalf("listing identity changed: %v", e)
					}
				}
			}
			if count != 1 {
				t.Fatalf("native folders: %d", count)
			}
			query := decodeMap(t, files.QueryByPath, fmt.Sprintf(`{"path":%q}`, "/"+tc.local))
			if query["entriesVO"].(map[string]any)["id"] != folderID {
				t.Fatalf("query disagrees: %v", query)
			}
		})
	}
}
