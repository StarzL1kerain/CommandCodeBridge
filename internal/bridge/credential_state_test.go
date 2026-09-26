package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

// 凭据文件被外部删掉后（例如在宿主的认证文件页里删的），列表必须自动清掉这条幽灵记录，
// 否则会变成"列表里还显示、删又删不掉"的死结。
func TestCredentialsPrunesCredentialsWhoseFileIsGone(t *testing.T) {
	dir := t.TempDir()
	keep := Credential{Type: Provider, ID: PluginID + "-keep", Label: "keep"}
	gone := Credential{Type: Provider, ID: PluginID + "-gone", Label: "gone"}
	if err := os.WriteFile(filepath.Join(dir, keep.ID+".json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewService()
	s.authDir = dir
	s.creds[keep.ID] = keep
	s.creds[gone.ID] = gone
	s.authFiles[keep.ID] = keep.ID + ".json"
	s.authFiles[gone.ID] = gone.ID + ".json"

	items := s.credentials()
	if len(items) != 1 || str(items[0]["id"]) != keep.ID {
		t.Fatalf("应只列出文件仍在的那条，实际 %v", items)
	}
	if _, ok := s.creds[gone.ID]; ok {
		t.Fatal("幽灵记录应从内存里清掉")
	}
	if _, ok := s.authFiles[gone.ID]; ok {
		t.Fatal("幽灵记录的文件名映射应清掉")
	}
	if _, ok := s.creds[keep.ID]; !ok {
		t.Fatal("文件还在的凭据不应被清掉")
	}
}

// 拿不到 auth-dir 时不能清理：宁可不清理，也不能把全部凭据误判成幽灵。
func TestCredentialsKeepsEverythingWhenAuthDirUnknown(t *testing.T) {
	s := NewService()
	s.creds["a"] = Credential{Type: Provider, ID: "a"}
	s.authFiles["a"] = "a.json"
	if items := s.credentials(); len(items) != 1 {
		t.Fatalf("auth-dir 未知时应原样列出，实际 %v", items)
	}
	if _, ok := s.creds["a"]; !ok {
		t.Fatal("auth-dir 未知时不应清理")
	}
}

// 文件已经不在了还去删，应当幂等成功并清掉内存记录，而不是回 409 把人卡住。
func TestDeleteCredentialIsIdempotentWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	gone := Credential{Type: Provider, ID: PluginID + "-gone"}
	s := NewService()
	s.authDir = dir
	s.creds[gone.ID] = gone
	s.authFiles[gone.ID] = gone.ID + ".json"

	res, err := s.deleteCredential(gone.ID)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	response, ok := res.(ManagementResponse)
	if !ok || response.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %#v", res)
	}
	if _, ok := s.creds[gone.ID]; ok {
		t.Fatal("内存里的记录应被清掉")
	}
	if _, ok := s.authFiles[gone.ID]; ok {
		t.Fatal("文件名映射应被清掉")
	}
}
