package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeDrop 는 dlddu/drop 의 서비스 계정 교환·조각 업로드·받기 계약을 그대로 흉내 낸다.
// 조각은 메모리에 쌓되, 테스트가 끼워 넣는 409 한 번(conflictAt)으로 이어 올리기를 시험한다.
type fakeDrop struct {
	t      *testing.T
	server *httptest.Server
	saTok  string

	mu        sync.Mutex
	saStatus  int
	keys      map[string]bool
	exchanges atomic.Int32
	initBody  map[string]any
	drops     map[string]*fakeDropEntry
	// conflictAt 이 0 이 아니면 그 offset 의 첫 PUT 에 409 를 내고, 서버가 "받은" 위치를
	// conflictJump 로 앞당긴다 — 이전 시도가 일부를 이미 보냈던 상황이다.
	conflictAt    int64
	conflictJump  int64
	conflicted    bool
	puts          []string
	getFailStatus int
	// redirectOnce 가 켜지면 첫 PUT 을 307 로 같은 주소에 돌려보낸다(Sablier 의 깨우기 동작).
	redirectOnce bool
	redirected   bool
}

type fakeDropEntry struct {
	password string
	key      string
	files    []dropFile
	data     [][]byte
	complete bool
}

func newFakeDrop(t *testing.T) *fakeDrop {
	t.Helper()
	f := &fakeDrop{t: t, saTok: "projected-sa-token", saStatus: http.StatusCreated,
		keys: map[string]bool{}, drops: map[string]*fakeDropEntry{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sa/token", func(w http.ResponseWriter, r *http.Request) {
		f.exchanges.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.saTok {
			writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "유효하지 않은 서비스 계정 토큰입니다"})
			return
		}
		f.mu.Lock()
		status := f.saStatus
		f.mu.Unlock()
		if status != http.StatusCreated {
			writeFakeJSON(w, status, map[string]any{"error": "허용되지 않은 서비스 계정입니다: preview/session-mcp"})
			return
		}
		key := "drk_" + randomHex(t, 8)
		f.mu.Lock()
		f.keys[key] = true
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusCreated, map[string]any{"token": key, "expires_at": "2026-11-01T01:00:00Z",
			"max_days": 30, "created_by": "sa:session-platform/session-mcp", "base_url": "https://drop.llkm.nl"})
	})
	mux.HandleFunc("POST /api/v1/uploads", f.keyed(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Files    []dropFile `json:"files"`
			ID       string     `json:"id"`
			Password string     `json:"password"`
			Days     int        `json:"days"`
		}
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.Unmarshal(raw, &f.initBody)
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Files) == 0 {
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "요청 형식이 잘못되었습니다"})
			return
		}
		id := body.ID
		if id == "" {
			id = "auto-" + strconv.Itoa(len(f.drops)+1)
		}
		if _, taken := f.drops[id]; taken {
			writeFakeJSON(w, http.StatusConflict, map[string]any{"error": "이미 쓰고 있는 아이디입니다"})
			return
		}
		pw := body.Password
		if pw == "" {
			pw = "gen-pass"
		}
		e := &fakeDropEntry{password: pw, key: "upkey" + id, files: body.Files, data: make([][]byte, len(body.Files))}
		f.drops[id] = e
		writeFakeJSON(w, http.StatusCreated, map[string]any{"id": id, "password": pw, "upload_key": e.key,
			"days": body.Days, "chunk_bytes": 4, "files": body.Files})
	}))
	mux.HandleFunc("PUT /api/v1/uploads/{id}/{key}/files/{n}", f.keyed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.redirectOnce && !f.redirected {
			f.redirected = true
			f.mu.Unlock()
			http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
			return
		}
		defer f.mu.Unlock()
		e, n := f.pending(w, r)
		if e == nil {
			return
		}
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		chunk, _ := io.ReadAll(r.Body)
		f.puts = append(f.puts, r.PathValue("n")+"@"+strconv.FormatInt(offset, 10))
		if f.conflictAt != 0 && offset == f.conflictAt && !f.conflicted {
			f.conflicted = true
			// 이전 시도가 conflictJump 까지 이미 보냈다 — 같은 원본 바이트로 채운다.
			e.data[n] = append(e.data[n], chunk[:f.conflictJump-offset]...)
			writeFakeJSON(w, http.StatusConflict, map[string]any{"error": "offset 이 맞지 않습니다", "received": len(e.data[n])})
			return
		}
		if offset != int64(len(e.data[n])) {
			writeFakeJSON(w, http.StatusConflict, map[string]any{"error": "offset 이 맞지 않습니다", "received": len(e.data[n])})
			return
		}
		e.data[n] = append(e.data[n], chunk...)
		writeFakeJSON(w, http.StatusOK, map[string]any{"received": len(e.data[n])})
	}))
	mux.HandleFunc("GET /api/v1/uploads/{id}/{key}/files/{n}", f.keyed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		e, n := f.pending(w, r)
		if e == nil {
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"received": len(e.data[n]), "size": e.files[n].Size})
	}))
	mux.HandleFunc("POST /api/v1/uploads/{id}/{key}/complete", f.keyed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		e := f.drops[r.PathValue("id")]
		if e == nil || e.key != r.PathValue("key") {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "찾을 수 없습니다"})
			return
		}
		var total int64
		for i, fm := range e.files {
			if int64(len(e.data[i])) != fm.Size {
				writeFakeJSON(w, http.StatusConflict, map[string]any{"error": "아직 다 올라가지 않았습니다"})
				return
			}
			total += fm.Size
		}
		e.complete = true
		writeFakeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"),
			"url": "https://drop.llkm.nl/get?id=" + r.PathValue("id"), "files": e.files, "bytes": total,
			"expires_at": "2026-11-08T00:00:00Z", "created_by": "sa:session-platform/session-mcp"})
	}))
	mux.HandleFunc("GET /get/{id}", func(w http.ResponseWriter, r *http.Request) {
		e := f.granted(w, r)
		if e == nil {
			return
		}
		var total int64
		for _, fm := range e.files {
			total += fm.Size
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "files": e.files, "bytes": total,
			"expires_at": "2026-11-08T00:00:00Z"})
	})
	mux.HandleFunc("GET /get/{id}/f/{n}", func(w http.ResponseWriter, r *http.Request) {
		e := f.granted(w, r)
		if e == nil {
			return
		}
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil || n < 0 || n >= len(e.data) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(e.data[n])
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (f *fakeDrop) keyed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok := f.keys[key]
		f.mu.Unlock()
		if !ok {
			writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "API 키가 필요합니다"})
			return
		}
		h(w, r)
	}
}

func (f *fakeDrop) pending(w http.ResponseWriter, r *http.Request) (*fakeDropEntry, int) {
	e := f.drops[r.PathValue("id")]
	n, err := strconv.Atoi(r.PathValue("n"))
	if e == nil || e.key != r.PathValue("key") || e.complete || err != nil || n < 0 || n >= len(e.files) {
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "찾을 수 없습니다"})
		return nil, 0
	}
	return e, n
}

func (f *fakeDrop) granted(w http.ResponseWriter, r *http.Request) *fakeDropEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getFailStatus != 0 {
		http.Error(w, "시도가 너무 많습니다. 15분 뒤에 다시 시도하세요.", f.getFailStatus)
		return nil
	}
	id, pw, ok := r.BasicAuth()
	e := f.drops[r.PathValue("id")]
	if !ok || e == nil || !e.complete || id != r.PathValue("id") || pw != e.password {
		http.Error(w, "아이디 또는 비밀번호가 맞지 않거나, 보관 기간이 끝났습니다.", http.StatusUnauthorized)
		return nil
	}
	return e
}

// seed 는 받기 테스트용으로 이미 닫힌 보관함 하나를 둔다.
func (f *fakeDrop) seed(id, password string, files map[string]string, order []string) {
	e := &fakeDropEntry{password: password, complete: true}
	for _, name := range order {
		e.files = append(e.files, dropFile{Name: name, Size: int64(len(files[name]))})
		e.data = append(e.data, []byte(files[name]))
	}
	f.mu.Lock()
	f.drops[id] = e
	f.mu.Unlock()
}

type dropMCP struct {
	server    *httptest.Server
	drop      *fakeDrop
	gateway   *fakeGateway
	sharedDir string
}

func newDropMCP(t *testing.T) *dropMCP {
	t.Helper()
	d := &dropMCP{drop: newFakeDrop(t), gateway: newFakeGateway(t, "APPROVED"), sharedDir: t.TempDir()}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(d.drop.saTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.server = newSessionMCPServerWith(t, sessionMCPConfig{
		gateway:   d.gateway.gate(t),
		sharedDir: d.sharedDir,
		drop:      newDropClient(d.drop.server.URL+"/", tokenFile),
	})
	return d
}

func (d *dropMCP) call(t *testing.T, tool string, args map[string]any) (bool, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 31, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, decoded := postMCP(t, d.server, string(body))
	if status != http.StatusOK {
		t.Fatalf("tools/call = %d, want 200", status)
	}
	_, isError, text := toolResult(t, decoded)
	return isError, text
}

func (d *dropMCP) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(d.sharedDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// AC-I1: 결과에는 메타데이터만 있고, 파일 내용은 결과 어디에도 없다.
func TestDropUploadSendsFilesInChunksAndReturnsOnlyMetadata(t *testing.T) {
	d := newDropMCP(t)
	d.write(t, "out/report.txt", "0123456789-secret-payload")
	d.write(t, "out/empty.bin", "")
	isError, text := d.call(t, dropUploadTool, map[string]any{
		"paths": []any{"out/report.txt", filepath.Join(d.sharedDir, "out/empty.bin")},
		"days":  3, "id": "e2e-report", "password": "hunter22",
	})
	if isError {
		t.Fatalf("upload failed: %s", text)
	}
	if strings.Contains(text, "secret-payload") {
		t.Fatalf("tool result carries file contents: %s", text)
	}
	var got dropUploadResult
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("result is not JSON: %v %s", err, text)
	}
	if got.ID != "e2e-report" || got.Password != "hunter22" || got.Bytes != 25 || len(got.Files) != 2 ||
		got.Files[0].Name != "report.txt" || got.URL == "" || got.ExpiresAt == "" {
		t.Fatalf("result = %+v", got)
	}
	e := d.drop.drops["e2e-report"]
	if !e.complete || string(e.data[0]) != "0123456789-secret-payload" || len(e.data[1]) != 0 {
		t.Fatalf("drop holds %q / %q complete=%v", e.data[0], e.data[1], e.complete)
	}
	// chunk_bytes 4 를 따라 조각으로 나갔다.
	if len(d.drop.puts) < 7 {
		t.Fatalf("puts = %v, want 4-byte chunks", d.drop.puts)
	}
	if d.drop.initBody["days"] != float64(3) {
		t.Fatalf("init body = %v", d.drop.initBody)
	}
	// AC-I3: 게이트웨이에 아무것도 가지 않았다.
	if n := d.gateway.created.Load(); n != 0 {
		t.Fatalf("approval requests = %d, want 0", n)
	}
}

func TestDropUploadResumesFromWhereDropSaysAfterA409(t *testing.T) {
	d := newDropMCP(t)
	d.drop.conflictAt = 4
	d.drop.conflictJump = 6
	d.drop.redirectOnce = true
	d.write(t, "a.txt", "abcdefghijkl")
	isError, text := d.call(t, dropUploadTool, map[string]any{"paths": []any{"a.txt"}})
	if isError {
		t.Fatalf("upload failed: %s", text)
	}
	e := d.drop.drops["auto-1"]
	if string(e.data[0]) != "abcdefghijkl" {
		t.Fatalf("drop holds %q, want the file byte for byte", e.data[0])
	}
	want := []string{"0@0", "0@4", "0@6", "0@10"}
	if strings.Join(d.drop.puts, ",") != strings.Join(want, ",") {
		t.Fatalf("puts = %v, want %v (resume at the received offset; the 307 replays the body)", d.drop.puts, want)
	}
	if !d.drop.redirected {
		t.Fatal("the redirect case did not run")
	}
}

func TestDropUploadExchangesAFreshKeyPerCall(t *testing.T) {
	d := newDropMCP(t)
	d.write(t, "a.txt", "x")
	for i := 0; i < 2; i++ {
		if isError, text := d.call(t, dropUploadTool, map[string]any{"paths": []any{"a.txt"}}); isError {
			t.Fatalf("upload %d failed: %s", i, text)
		}
	}
	if n := d.drop.exchanges.Load(); n != 2 {
		t.Fatalf("exchanges = %d, want one per call", n)
	}
}

func TestDropUploadRejectsPathsOutsideTheVolume(t *testing.T) {
	d := newDropMCP(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "passwd"), filepath.Join(d.sharedDir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(d.sharedDir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.sharedDir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"dotdot", map[string]any{"paths": []any{"../passwd"}}},
		{"dotdot in the middle", map[string]any{"paths": []any{"a/../../passwd"}}},
		{"absolute outside", map[string]any{"paths": []any{filepath.Join(outside, "passwd")}}},
		{"symlink out", map[string]any{"paths": []any{"link"}}},
		{"through a symlinked dir", map[string]any{"paths": []any{"dirlink/passwd"}}},
		{"directory", map[string]any{"paths": []any{"adir"}}},
		{"volume root", map[string]any{"paths": []any{d.sharedDir}}},
		{"missing", map[string]any{"paths": []any{"nope"}}},
		{"no paths", map[string]any{"paths": []any{}}},
		{"duplicate", map[string]any{"paths": []any{"adir/../x", "x"}}},
		{"bad days", map[string]any{"paths": []any{"x"}, "days": 31}},
		{"zero days", map[string]any{"paths": []any{"x"}, "days": 0}},
		{"bad id", map[string]any{"paths": []any{"x"}, "id": "-Nope"}},
		{"short password", map[string]any{"paths": []any{"x"}, "password": "abc"}},
		{"unknown shape", map[string]any{"paths": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if isError, text := d.call(t, dropUploadTool, tc.args); !isError {
				t.Fatalf("accepted: %s", text)
			}
		})
	}
	if n := d.drop.exchanges.Load(); n != 0 {
		t.Fatalf("exchanges = %d — a rejected call must not reach drop", n)
	}
	if len(d.drop.drops) != 0 {
		t.Fatalf("drops = %v", d.drop.drops)
	}
}

func TestDropUploadFailsClosedWhenTheServiceAccountIsNotAllowed(t *testing.T) {
	d := newDropMCP(t)
	d.drop.saStatus = http.StatusForbidden
	d.write(t, "a.txt", "x")
	isError, text := d.call(t, dropUploadTool, map[string]any{"paths": []any{"a.txt"}})
	if !isError || !strings.Contains(text, "403") || !strings.Contains(text, "SA_ALLOW") {
		t.Fatalf("result = %v %s, want a 403 naming the allow list", isError, text)
	}
	if strings.Contains(text, d.drop.saTok) {
		t.Fatal("the service account token leaked into the result")
	}
}

func TestDropUploadReportsATakenID(t *testing.T) {
	d := newDropMCP(t)
	d.drop.seed("taken-id", "pw-pw-pw", map[string]string{"a": "x"}, []string{"a"})
	d.write(t, "a.txt", "x")
	isError, text := d.call(t, dropUploadTool, map[string]any{"paths": []any{"a.txt"}, "id": "taken-id"})
	if !isError || !strings.Contains(text, "이미 쓰고 있다") {
		t.Fatalf("result = %v %s", isError, text)
	}
}

func TestDropDownloadWritesFilesOntoTheVolumeAndReturnsOnlyPaths(t *testing.T) {
	d := newDropMCP(t)
	d.drop.seed("inbox-1", "s3cret-pw", map[string]string{"notes.md": "hello-contents", "data.csv": "a,b\n1,2\n"},
		[]string{"notes.md", "data.csv"})
	isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "inbox-1", "password": "s3cret-pw"})
	if isError {
		t.Fatalf("download failed: %s", text)
	}
	if strings.Contains(text, "hello-contents") {
		t.Fatalf("tool result carries file contents: %s", text)
	}
	var got struct {
		Dest  string               `json:"dest"`
		Files []dropDownloadedFile `json:"files"`
		Bytes int64                `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(d.sharedDir, "drop", "inbox-1")
	if got.Dest != wantDir || len(got.Files) != 2 || got.Bytes != 22 {
		t.Fatalf("result = %+v", got)
	}
	for _, f := range got.Files {
		raw, err := os.ReadFile(f.Path)
		if err != nil || int64(len(raw)) != f.Size {
			t.Fatalf("%s: %v (%d bytes, want %d)", f.Path, err, len(raw), f.Size)
		}
	}
	entries, _ := os.ReadDir(wantDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
	if n := d.gateway.created.Load(); n != 0 {
		t.Fatalf("approval requests = %d, want 0", n)
	}
	if n := d.drop.exchanges.Load(); n != 0 {
		t.Fatalf("downloads need no key; exchanges = %d", n)
	}
}

func TestDropDownloadPicksFilesByNameOrIndex(t *testing.T) {
	d := newDropMCP(t)
	d.drop.seed("pick-me", "pw-pw-pw", map[string]string{"a": "A", "b": "B", "c": "C"}, []string{"a", "b", "c"})
	isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "pick-me", "password": "pw-pw-pw",
		"files": []any{"c", 0}, "dest": "in"})
	if isError {
		t.Fatalf("download failed: %s", text)
	}
	entries, _ := os.ReadDir(filepath.Join(d.sharedDir, "in"))
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want a and c", entries)
	}
	for _, bad := range []any{"zzz", 3, -1, 1.5, true} {
		if isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "pick-me", "password": "pw-pw-pw",
			"files": []any{bad}, "dest": "other"}); !isError {
			t.Fatalf("selector %v accepted: %s", bad, text)
		}
	}
}

// drop 이 돌려주는 이름은 믿지 않는다: 어떤 이름도 대상 디렉터리 밖에 파일을 만들지 못한다.
func TestDropDownloadSanitizesNamesFromDrop(t *testing.T) {
	d := newDropMCP(t)
	names := []string{"../../escape", "/etc/passwd", "..", ".hidden", "a\\b", "ok.txt", "ok.txt", "", "x\x00y"}
	files := map[string]string{}
	for _, n := range names {
		files[n] = "z"
	}
	e := &fakeDropEntry{password: "pw-pw-pw", complete: true}
	for _, n := range names {
		e.files = append(e.files, dropFile{Name: n, Size: 1})
		e.data = append(e.data, []byte("z"))
	}
	d.drop.drops["evil-names"] = e
	isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "evil-names", "password": "pw-pw-pw"})
	if isError {
		t.Fatalf("download failed: %s", text)
	}
	var got struct {
		Files []dropDownloadedFile `json:"files"`
	}
	_ = json.Unmarshal([]byte(text), &got)
	dir := filepath.Join(d.sharedDir, "drop", "evil-names")
	if len(got.Files) != len(names) {
		t.Fatalf("files = %+v", got.Files)
	}
	seen := map[string]bool{}
	for _, f := range got.Files {
		if filepath.Dir(f.Path) != dir {
			t.Fatalf("%q escaped the destination directory", f.Path)
		}
		if strings.HasPrefix(filepath.Base(f.Path), ".") {
			t.Fatalf("%q is a dot file", f.Path)
		}
		if seen[f.Path] {
			t.Fatalf("two files written to %q", f.Path)
		}
		seen[f.Path] = true
	}
	// 볼륨 바깥 쪽에 아무것도 생기지 않았다.
	if _, err := os.Stat(filepath.Join(d.sharedDir, "escape")); err == nil {
		t.Fatal("a name escaped onto the volume root")
	}
}

func TestDropDownloadDoesNotOverwriteUnlessAsked(t *testing.T) {
	d := newDropMCP(t)
	d.drop.seed("keep-mine", "pw-pw-pw", map[string]string{"a.txt": "NEW", "b.txt": "B"}, []string{"a.txt", "b.txt"})
	d.write(t, "in/a.txt", "MINE")
	isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "keep-mine", "password": "pw-pw-pw", "dest": "in"})
	if !isError || !strings.Contains(text, "overwrite") {
		t.Fatalf("result = %v %s, want a refusal", isError, text)
	}
	if raw, _ := os.ReadFile(filepath.Join(d.sharedDir, "in/a.txt")); string(raw) != "MINE" {
		t.Fatalf("a.txt = %q, want it untouched", raw)
	}
	if _, err := os.Stat(filepath.Join(d.sharedDir, "in/b.txt")); err == nil {
		t.Fatal("b.txt was written although the call was refused")
	}
	isError, text = d.call(t, dropDownloadTool, map[string]any{"id": "keep-mine", "password": "pw-pw-pw",
		"dest": "in", "overwrite": true})
	if isError {
		t.Fatalf("overwrite failed: %s", text)
	}
	if raw, _ := os.ReadFile(filepath.Join(d.sharedDir, "in/a.txt")); string(raw) != "NEW" {
		t.Fatalf("a.txt = %q, want the downloaded bytes", raw)
	}
}

// 덮어쓰기라도 대상이 심볼릭 링크면 링크 너머가 아니라 링크 자체가 바뀐다.
func TestDropDownloadOverwriteReplacesASymlinkNotItsTarget(t *testing.T) {
	d := newDropMCP(t)
	d.drop.seed("link-swap", "pw-pw-pw", map[string]string{"a.txt": "NEW"}, []string{"a.txt"})
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.sharedDir, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(d.sharedDir, "in/a.txt")); err != nil {
		t.Fatal(err)
	}
	if isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "link-swap", "password": "pw-pw-pw",
		"dest": "in", "overwrite": true}); isError {
		t.Fatalf("overwrite failed: %s", text)
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "KEEP" {
		t.Fatalf("the file behind the symlink changed to %q", raw)
	}
}

func TestDropDownloadRejectsDestinationsOutsideTheVolume(t *testing.T) {
	d := newDropMCP(t)
	d.drop.seed("dest-test", "pw-pw-pw", map[string]string{"a": "A"}, []string{"a"})
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(d.sharedDir, "out")); err != nil {
		t.Fatal(err)
	}
	d.write(t, "afile", "x")
	for _, dest := range []string{"../x", outside, "out", "out/sub", "afile", "/"} {
		if isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "dest-test", "password": "pw-pw-pw",
			"dest": dest}); !isError {
			t.Fatalf("dest %q accepted: %s", dest, text)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was written outside the volume: %v", entries)
	}
}

func TestDropDownloadMapsDropRefusals(t *testing.T) {
	d := newDropMCP(t)
	d.drop.seed("guarded", "right-pw", map[string]string{"a": "A"}, []string{"a"})
	isError, text := d.call(t, dropDownloadTool, map[string]any{"id": "guarded", "password": "wrong-pw"})
	if !isError || !strings.Contains(text, "401") || strings.Contains(text, "right-pw") {
		t.Fatalf("wrong password: %v %s", isError, text)
	}
	d.drop.getFailStatus = http.StatusTooManyRequests
	isError, text = d.call(t, dropDownloadTool, map[string]any{"id": "guarded", "password": "right-pw"})
	if !isError || !strings.Contains(text, "429") {
		t.Fatalf("rate limit: %v %s", isError, text)
	}
	for _, args := range []map[string]any{
		{"id": "UP", "password": "x"},
		{"id": "guarded"},
		{"id": "guarded", "password": "x", "dest": "../x"},
	} {
		if isError, text := d.call(t, dropDownloadTool, args); !isError {
			t.Fatalf("%v accepted: %s", args, text)
		}
	}
}

func TestDropToolsAreListedOnlyWhereDropIsConfigured(t *testing.T) {
	names := func(c sessionMCPConfig) map[string]bool {
		out := map[string]bool{}
		for _, raw := range c.toolDefinitions() {
			out[raw.(map[string]any)["name"].(string)] = true
		}
		return out
	}
	gate := newFakeGateway(t, "APPROVED").gate(t)
	with := names(sessionMCPConfig{gateway: gate, drop: newDropClient("http://drop.example", "")})
	if !with[dropUploadTool] || !with[dropDownloadTool] {
		t.Fatalf("tools = %v, want both drop tools", with)
	}
	if without := names(sessionMCPConfig{gateway: gate}); without[dropUploadTool] || without[dropDownloadTool] {
		t.Fatalf("tools = %v, want no drop tools without DROP_URL", without)
	}
	// R2: 게이트가 없으면 drop 이 설정돼 있어도 표면이 빈다.
	if ungated := names(sessionMCPConfig{drop: newDropClient("http://drop.example", "")}); len(ungated) != 0 {
		t.Fatalf("tools = %v, want an empty surface without a gate", ungated)
	}
	for _, raw := range dropToolDefinitions() {
		schema := raw.(map[string]any)["inputSchema"].(map[string]any)
		if schema["additionalProperties"] != false {
			t.Errorf("%v: want additionalProperties false", raw.(map[string]any)["name"])
		}
	}
	if c := newDropClient("  ", "x"); c != nil {
		t.Fatal("an empty DROP_URL must leave drop off")
	}
	if c := newDropClient("http://d/", ""); c.tokenFile != defaultDropTokenFile || c.baseURL != "http://d" {
		t.Fatalf("client = %+v", c)
	}
}

func TestDropToolsSayWhenUnconfigured(t *testing.T) {
	srv := newSessionMCPServerWith(t, sessionMCPConfig{gateway: newFakeGateway(t, "APPROVED").gate(t), sharedDir: t.TempDir()})
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{dropUploadTool, map[string]any{"paths": []any{"a"}}},
		{dropDownloadTool, map[string]any{"id": "abc", "password": "pw"}},
	} {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tc.tool, "arguments": tc.args}})
		_, decoded := postMCP(t, srv, string(body))
		if _, isError, text := toolResult(t, decoded); !isError || !strings.Contains(text, "설정되어 있지 않다") {
			t.Fatalf("%s: %v %s", tc.tool, isError, text)
		}
	}
}
