// `drop_upload`·`drop_download` — 세션 MCP 가 같은 운영자의 클러스터 안 파일 전달 서비스
// drop(dlddu/drop)으로 공유 볼륨의 파일을 내보내고 받아 오는 두 도구
// (docs/prd/drop-file-share.md AC-I1~I4).
//
// 인자와 결과에는 경로·메타데이터만 실린다. 바이트는 이 컨테이너가 공유 볼륨과 drop 사이에서
// 직접 옮긴다 — 파일 내용이 MCP 인자·결과를 지나가면 그만큼 대화 컨텍스트를 태운다(AC-I1).
// 두 도구는 승인 게이트를 지나지 않는다. R1 의 명시적 예외이고 조건은
// docs/session-mcp-tool-surface.md 의 R1 항목이 갖는다.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// control-plane/internal/adapter/k8s 의 DropURLEnvVar·DropTokenFileEnvVar 와 맞춘다.
	dropURLEnv           = "DROP_URL"
	dropTokenFileEnv     = "DROP_TOKEN_FILE"
	defaultDropTokenFile = "/var/run/secrets/drop/token"

	dropUploadTool   = "drop_upload"
	dropDownloadTool = "drop_download"

	dropSATokenPath = "/api/v1/sa/token"
	dropUploadsPath = "/api/v1/uploads"
	dropGetPath     = "/get/"

	// 인클러스터 주소는 Traefik → Sablier 를 지나고, 잠든 drop 을 깨우는 첫 요청은 최대 1분
	// 가까이 막힌다(drop README). 메타데이터 요청 하나에 그보다 넉넉한 시간을 준다.
	dropRequestTimeout = 90 * time.Second
	// 조각 하나(아래 상한)의 전송 상한. 깨우기 대기에 16 MiB 전송이 더해진다.
	dropChunkTimeout = 5 * time.Minute
	// 파일 하나를 받는 동안 바이트가 이만큼 멈추면 끊는다. 2 GiB 파일 전체에 하나의 마감을
	// 걸면 느린 링크에서 정상 전송을 자르게 되므로, 전체 마감 대신 정체를 잰다.
	dropStallTimeout = 2 * time.Minute
	// drop 이 init 응답으로 주는 chunk_bytes 를 따르되, 서버 상한(64 MiB) 밑으로 이 값에서 자른다.
	dropMaxChunkBytes = 16 << 20
	// 같은 offset 에서 409 가 거듭되면 서버와 합의가 안 되는 것이라 무한 반복하지 않는다.
	dropMaxResumes = 8

	dropDefaultDays   = 7
	dropMaxDays       = 30
	dropMaxFiles      = 100
	dropSAKeyMinutes  = 60
	dropDownloadsRoot = "drop"
	maxDropJSONBytes  = 1 << 20
	maxDropErrorChars = 200
)

// drop 의 아이디 규칙(dlddu/drop store.go idRe)과 같다. 여기서 먼저 걸러 잘못된 아이디가
// 토큰 교환까지 가지 않게 한다(R3).
var dropIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,39}$`)

// dropClient 는 이 헬퍼 파드가 drop 을 부르는 자리다. baseURL 이 비면 기능이 꺼진 배포다.
type dropClient struct {
	baseURL   string
	tokenFile string
	client    *http.Client
}

// newDropClient 는 주소가 없으면 nil 을 돌려준다 — 그 배포의 도구 목록에는 drop 도구가 없다.
func newDropClient(baseURL, tokenFile string) *dropClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	tokenFile = strings.TrimSpace(tokenFile)
	if tokenFile == "" {
		tokenFile = defaultDropTokenFile
	}
	// 클라이언트 전체 Timeout 은 두지 않는다 — 요청마다 컨텍스트 마감을 따로 건다(위 상수).
	return &dropClient{baseURL: baseURL, tokenFile: tokenFile, client: &http.Client{}}
}

type dropFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// dropAPIError 는 drop 이 낸 상태와 메시지다. 메시지는 drop 이 쓴 한국어 문장이고 비밀을 담지 않는다.
type dropAPIError struct {
	status  int
	message string
}

func (e *dropAPIError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("drop 이 %d 를 돌려줬다", e.status)
	}
	return fmt.Sprintf("drop 이 %d 를 돌려줬다: %s", e.status, e.message)
}

// do 는 요청 하나를 보내고 JSON 본문을 out 에 푼다. 상태가 want 가 아니면 *dropAPIError 다.
// 본문은 GetBody 로 다시 만들 수 있어야 한다 — Sablier 의 307 을 따라갈 때 본문을 다시 보낸다.
func (c *dropClient) do(req *http.Request, want int, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("drop 이 제한 시간 안에 답하지 않았다")
		}
		return errors.New("drop 에 닿지 못했다")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDropJSONBytes))
	if err != nil {
		return errors.New("drop 의 응답을 읽지 못했다")
	}
	if resp.StatusCode != want {
		return &dropAPIError{status: resp.StatusCode, message: dropErrorText(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("drop 이 JSON 이 아닌 응답을 돌려줬다")
	}
	return nil
}

// dropErrorText 는 `{"error":"…"}` 의 문장을 꺼낸다. drop 의 받기 경로는 평문으로 답하므로
// 그 경우엔 첫 줄을 쓴다. 어느 쪽이든 길이를 자른다 — 원격이 정하는 글이다.
func dropErrorText(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	text := ""
	if json.Unmarshal(raw, &e) == nil {
		text = e.Error
	} else if utf8.Valid(raw) {
		text, _, _ = strings.Cut(string(raw), "\n")
	}
	return truncateText(strings.TrimSpace(text), maxDropErrorChars)
}

func jsonRequest(ctx context.Context, method, target string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		// bytes.Reader 라서 net/http 가 GetBody 를 채운다(307 재전송).
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// exchangeKey 는 projected 토큰(audience drop)을 drk_ 단기 키로 바꾼다. 키는 캐시하지 않고
// 업로드마다 새로 받는다: 교환은 싸고, 키가 이 프로세스 메모리에 호출 사이로 남지 않으며,
// 만료 경계(키가 업로드 도중 만료되는 경우)를 다룰 코드가 없어진다.
func (c *dropClient) exchangeKey(ctx context.Context, sessionID string, maxDays int) (string, error) {
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", errors.New("drop 용 서비스 어카운트 토큰을 읽지 못했다")
	}
	saToken := strings.TrimSpace(string(raw))
	if saToken == "" {
		return "", errors.New("drop 용 서비스 어카운트 토큰이 비어 있다")
	}
	ctx, cancel := context.WithTimeout(ctx, dropRequestTimeout)
	defer cancel()
	label := "session-mcp"
	if sessionID != "" {
		label += " " + sessionID
	}
	req, err := jsonRequest(ctx, http.MethodPost, c.baseURL+dropSATokenPath, map[string]any{
		"valid_minutes": dropSAKeyMinutes, "max_days": maxDays, "label": truncateText(label, 60),
	})
	if err != nil {
		return "", errors.New("drop 주소가 올바르지 않다")
	}
	req.Header.Set("Authorization", "Bearer "+saToken)
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(req, http.StatusCreated, &out); err != nil {
		var apiErr *dropAPIError
		if errors.As(err, &apiErr) {
			switch apiErr.status {
			case http.StatusUnauthorized:
				return "", errors.New("drop 이 이 헬퍼 파드의 서비스 어카운트 토큰을 받지 않았다(401)")
			case http.StatusForbidden:
				// 프리뷰 네임스페이스가 여기로 온다 — drop 의 SA_ALLOW 에 일부러 넣지 않았다.
				return "", errors.New("이 배포의 session-mcp 서비스 어카운트는 drop 의 허용 목록에 없다(403) — " +
					"drop 의 SA_ALLOW 에 <네임스페이스>/session-mcp 를 넣어야 쓸 수 있다")
			case http.StatusServiceUnavailable:
				return "", errors.New("drop 에 서비스 어카운트 교환이 설정되어 있지 않다(503)")
			}
		}
		return "", fmt.Errorf("drop 키를 받지 못했다: %w", err)
	}
	if !strings.HasPrefix(out.Token, "drk_") {
		return "", errors.New("drop 이 키가 아닌 값을 돌려줬다")
	}
	return out.Token, nil
}

// ---- 공유 볼륨 경로 ----

// sharedRelPath 는 에이전트가 준 경로를 공유 볼륨 기준 상대 경로로 바꾼다. 볼륨의 마운트
// 경로로 시작하는 절대 경로도 받는다(에이전트는 같은 볼륨을 같은 경로에 갖는다, AC-F5).
// `..` 조각·볼륨 밖 절대 경로는 여기서 거절하고, 볼륨 밖을 가리키는 심볼릭 링크는 os.Root 가 막는다.
func sharedRelPath(sharedDir, raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("경로가 비어 있다")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("경로에 NUL 문자가 있다")
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return "", fmt.Errorf("%q: 경로에 `..` 를 쓸 수 없다", raw)
		}
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(filepath.Clean(sharedDir), filepath.Clean(p))
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("%q: 공유 볼륨(%s) 밖의 경로다", raw, sharedDir)
		}
		p = rel
	}
	p = filepath.Clean(p)
	if p == "." || !filepath.IsLocal(p) {
		return "", fmt.Errorf("%q: 공유 볼륨 안의 파일을 가리키지 않는다", raw)
	}
	return p, nil
}

// mkdirAllIn 은 os.Root 안에서 디렉터리를 차례로 만든다(Go 1.24 의 Root 에는 MkdirAll 이 없다).
// 조각마다 Root 를 거치므로 중간에 볼륨 밖을 가리키는 링크가 있으면 거기서 멈춘다.
func mkdirAllIn(root *os.Root, rel string) error {
	cur := ""
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		err := root.Mkdir(cur, 0o755)
		if err == nil || errors.Is(err, os.ErrExist) {
			info, statErr := root.Stat(cur)
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("%s 는 디렉터리가 아니다", cur)
			}
			continue
		}
		return err
	}
	return nil
}

// dropSafeName 은 drop 이 돌려준 파일 이름을 대상 디렉터리 안의 한 조각으로 만든다. drop 도 이름을
// 정리하지만(cleanFileName) 그것을 믿지 않는다 — 구분자·제어 문자는 바꾸고, 점으로 시작하는 이름은
// 앞에 `_` 를 붙인다(이 도구의 임시 파일이 점으로 시작한다).
func dropSafeName(name string, index int) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '_'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	name = strings.TrimSpace(name)
	if len(name) > 200 {
		name = name[:validUTF8PrefixAtMost([]byte(name), 200)]
	}
	if strings.HasPrefix(name, ".") {
		name = "_" + name
	}
	if name == "" || name == "_." || name == "_.." || !filepath.IsLocal(name) {
		name = "file-" + strconv.Itoa(index)
	}
	return name
}

// ---- drop_upload ----

type dropUploadArgs struct {
	Paths    []string `json:"paths"`
	Days     *int     `json:"days"`
	ID       string   `json:"id"`
	Password string   `json:"password"`
}

type openedUpload struct {
	rel  string
	file *os.File
	size int64
}

func (c sessionMCPConfig) callDropUpload(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	var args dropUploadArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return mcpToolError("인자는 paths(문자열 배열)와 선택 항목 days·id·password 를 가진 객체여야 한다"), nil
		}
	}
	days := dropDefaultDays
	if args.Days != nil {
		days = *args.Days
	}
	if days < 1 || days > dropMaxDays {
		return mcpToolError(fmt.Sprintf("days 는 1~%d 이다", dropMaxDays)), nil
	}
	if len(args.Paths) == 0 || len(args.Paths) > dropMaxFiles {
		return mcpToolError(fmt.Sprintf("paths 는 1~%d 개다", dropMaxFiles)), nil
	}
	id := strings.ToLower(strings.TrimSpace(args.ID))
	if id != "" && !dropIDPattern.MatchString(id) {
		return mcpToolError("id 는 영문 소문자·숫자·하이픈 3~40자(첫 글자는 하이픈이 아님)다"), nil
	}
	password := strings.TrimSpace(args.Password)
	if password != "" && (utf8.RuneCountInString(password) < 6 || len(password) > 128) {
		return mcpToolError("password 는 6자 이상 128바이트 이하다"), nil
	}
	if c.drop == nil {
		return mcpToolError("이 배포에는 drop 이 설정되어 있지 않다"), nil
	}
	if c.sharedDir == "" {
		return mcpToolError("이 세션에는 공유 볼륨이 없어서 올릴 파일을 읽을 자리가 없다"), nil
	}

	root, err := os.OpenRoot(c.sharedDir)
	if err != nil {
		return mcpToolError("공유 볼륨을 열지 못했다: " + err.Error()), nil
	}
	defer root.Close()
	opened := make([]openedUpload, 0, len(args.Paths))
	defer func() {
		for _, o := range opened {
			o.file.Close()
		}
	}()
	seen := map[string]bool{}
	files := make([]dropFile, 0, len(args.Paths))
	for _, raw := range args.Paths {
		rel, err := sharedRelPath(c.sharedDir, raw)
		if err != nil {
			return mcpToolError(err.Error()), nil
		}
		if seen[rel] {
			return mcpToolError(fmt.Sprintf("%q 를 두 번 줬다", raw)), nil
		}
		seen[rel] = true
		// 디렉터리는 받지 않는다: 무엇이 나가는지가 에이전트가 이름 붙인 파일 그대로여야 한다.
		f, err := root.Open(rel)
		if err != nil {
			return mcpToolError(fmt.Sprintf("%q 를 열지 못했다(없거나 공유 볼륨 밖을 가리킨다)", raw)), nil
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return mcpToolError(fmt.Sprintf("%q 의 정보를 읽지 못했다", raw)), nil
		}
		if !info.Mode().IsRegular() {
			f.Close()
			return mcpToolError(fmt.Sprintf("%q 는 일반 파일이 아니다 — 디렉터리는 안의 파일을 하나씩 paths 에 넣는다", raw)), nil
		}
		opened = append(opened, openedUpload{rel: rel, file: f, size: info.Size()})
		files = append(files, dropFile{Name: filepath.Base(rel), Size: info.Size()})
	}

	sessionID := ""
	if c.gateway != nil {
		sessionID = c.gateway.sessionID
	}
	key, err := c.drop.exchangeKey(ctx, sessionID, days)
	if err != nil {
		logger.Error("drop key exchange failed", "tool", dropUploadTool, "err", err)
		return mcpToolError("올리지 못했다: " + err.Error()), nil
	}
	logger.Info("uploading to drop", "tool", dropUploadTool, "files", len(files))
	result, err := c.drop.upload(ctx, key, files, opened, id, password, days)
	if err != nil {
		logger.Error("drop upload failed", "tool", dropUploadTool, "err", err)
		return mcpToolError("올리지 못했다: " + err.Error()), nil
	}
	logger.Info("uploaded to drop", "tool", dropUploadTool, "id", result.ID, "files", len(result.Files), "bytes", result.Bytes)
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	return mcpToolText(string(payload)), nil
}

type dropUploadResult struct {
	ID        string     `json:"id"`
	Password  string     `json:"password"`
	URL       string     `json:"url"`
	Files     []dropFile `json:"files"`
	Bytes     int64      `json:"bytes"`
	ExpiresAt string     `json:"expires_at"`
}

// upload 는 늘 조각 API 를 쓴다: 파일을 메모리에 올리지 않고 디스크에서 바로 흘려보내며,
// 409 를 받으면 서버가 받은 자리부터 이어 보낸다.
func (c *dropClient) upload(ctx context.Context, key string, files []dropFile, opened []openedUpload, id, password string, days int) (dropUploadResult, error) {
	initBody := map[string]any{"files": files, "days": days}
	if id != "" {
		initBody["id"] = id
	}
	if password != "" {
		initBody["password"] = password
	}
	var started struct {
		ID         string     `json:"id"`
		Password   string     `json:"password"`
		UploadKey  string     `json:"upload_key"`
		ChunkBytes int64      `json:"chunk_bytes"`
		Files      []dropFile `json:"files"`
	}
	if err := c.keyed(ctx, key, http.MethodPost, c.baseURL+dropUploadsPath, initBody, http.StatusCreated, &started); err != nil {
		var apiErr *dropAPIError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusConflict {
			return dropUploadResult{}, errors.New("그 id 는 이미 쓰고 있다 — 다른 id 를 주거나 비워 둔다")
		}
		return dropUploadResult{}, fmt.Errorf("보관함을 열지 못했다: %w", err)
	}
	if started.ID == "" || started.UploadKey == "" || len(started.Files) != len(files) {
		return dropUploadResult{}, errors.New("drop 의 업로드 시작 응답이 예상과 다르다")
	}
	chunk := started.ChunkBytes
	if chunk <= 0 || chunk > dropMaxChunkBytes {
		chunk = dropMaxChunkBytes
	}
	base := c.baseURL + dropUploadsPath + "/" + url.PathEscape(started.ID) + "/" + url.PathEscape(started.UploadKey) + "/files/"
	for n, o := range opened {
		if err := c.sendFile(ctx, key, base+strconv.Itoa(n), o, chunk); err != nil {
			return dropUploadResult{}, fmt.Errorf("%s: %w", o.rel, err)
		}
	}
	var done struct {
		ID        string     `json:"id"`
		URL       string     `json:"url"`
		Files     []dropFile `json:"files"`
		Bytes     int64      `json:"bytes"`
		ExpiresAt string     `json:"expires_at"`
	}
	completeURL := c.baseURL + dropUploadsPath + "/" + url.PathEscape(started.ID) + "/" + url.PathEscape(started.UploadKey) + "/complete"
	if err := c.keyed(ctx, key, http.MethodPost, completeURL, nil, http.StatusOK, &done); err != nil {
		return dropUploadResult{}, fmt.Errorf("보관함을 닫지 못했다: %w", err)
	}
	// complete 응답에는 비밀번호가 없다 — init 응답의 것을 쓴다.
	return dropUploadResult{
		ID: done.ID, Password: started.Password, URL: done.URL,
		Files: done.Files, Bytes: done.Bytes, ExpiresAt: done.ExpiresAt,
	}, nil
}

// keyed 는 drk_ 키로 메타데이터 요청 하나를 보낸다.
func (c *dropClient) keyed(ctx context.Context, key, method, target string, body any, want int, out any) error {
	ctx, cancel := context.WithTimeout(ctx, dropRequestTimeout)
	defer cancel()
	req, err := jsonRequest(ctx, method, target, body)
	if err != nil {
		return errors.New("drop 주소가 올바르지 않다")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return c.do(req, want, out)
}

// sendFile 은 파일 하나를 조각으로 보낸다. 조각 본문은 파일의 구간(SectionReader)이라 메모리에
// 쌓이지 않고, GetBody 가 같은 구간을 다시 열어 307 을 따라갈 수 있다.
func (c *dropClient) sendFile(ctx context.Context, key, target string, o openedUpload, chunk int64) error {
	var offset int64
	resumes := 0
	for offset < o.size {
		n := min(chunk, o.size-offset)
		received, conflict, err := c.putChunk(ctx, key, target, o.file, offset, n)
		if err != nil {
			return err
		}
		if conflict {
			resumes++
			if resumes > dropMaxResumes {
				return errors.New("drop 과 이어 올릴 위치가 맞지 않는다")
			}
		}
		if received < 0 || received > o.size {
			return errors.New("drop 이 파일 크기 밖의 위치를 돌려줬다")
		}
		offset = received
	}
	return nil
}

func (c *dropClient) putChunk(ctx context.Context, key, target string, file *os.File, offset, n int64) (received int64, conflict bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, dropChunkTimeout)
	defer cancel()
	body := func() io.ReadCloser { return io.NopCloser(io.NewSectionReader(file, offset, n)) }
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target+"?offset="+strconv.FormatInt(offset, 10), body())
	if err != nil {
		return 0, false, errors.New("drop 주소가 올바르지 않다")
	}
	req.ContentLength = n
	req.GetBody = func() (io.ReadCloser, error) { return body(), nil }
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	var out struct {
		Received *int64 `json:"received"`
	}
	err = c.do(req, http.StatusOK, &out)
	var apiErr *dropAPIError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusConflict {
		// 409 본문의 received 가 이어 올릴 위치다. 본문은 do 가 이미 읽었으므로 한 번 더 묻는다.
		got, statusErr := c.chunkStatus(ctx, key, target)
		if statusErr != nil {
			return 0, false, statusErr
		}
		return got, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if out.Received == nil {
		return 0, false, errors.New("drop 의 조각 응답에 received 가 없다")
	}
	return *out.Received, false, nil
}

func (c *dropClient) chunkStatus(ctx context.Context, key, target string) (int64, error) {
	var out struct {
		Received *int64 `json:"received"`
	}
	if err := c.keyed(ctx, key, http.MethodGet, target, nil, http.StatusOK, &out); err != nil {
		return 0, fmt.Errorf("이어 올릴 위치를 묻지 못했다: %w", err)
	}
	if out.Received == nil {
		return 0, errors.New("drop 의 상태 응답에 received 가 없다")
	}
	return *out.Received, nil
}

// ---- drop_download ----

type dropDownloadArgs struct {
	ID        string            `json:"id"`
	Password  string            `json:"password"`
	Files     []json.RawMessage `json:"files"`
	Dest      string            `json:"dest"`
	Overwrite bool              `json:"overwrite"`
}

type dropDownloadedFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type dropListing struct {
	ID        string     `json:"id"`
	Files     []dropFile `json:"files"`
	Bytes     int64      `json:"bytes"`
	ExpiresAt string     `json:"expires_at"`
}

func (c sessionMCPConfig) callDropDownload(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	var args dropDownloadArgs
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return mcpToolError("인자는 id·password 와 선택 항목 files·dest·overwrite 를 가진 객체여야 한다"), nil
		}
	}
	id := strings.ToLower(strings.TrimSpace(args.ID))
	if !dropIDPattern.MatchString(id) {
		return mcpToolError("id 는 영문 소문자·숫자·하이픈 3~40자다"), nil
	}
	if args.Password == "" {
		return mcpToolError("password 가 필요하다"), nil
	}
	if len(args.Files) > dropMaxFiles {
		return mcpToolError(fmt.Sprintf("files 는 %d 개까지다", dropMaxFiles)), nil
	}
	if c.drop == nil {
		return mcpToolError("이 배포에는 drop 이 설정되어 있지 않다"), nil
	}
	if c.sharedDir == "" {
		return mcpToolError("이 세션에는 공유 볼륨이 없어서 받은 파일을 둘 자리가 없다"), nil
	}
	dest := filepath.Join(dropDownloadsRoot, id)
	if strings.TrimSpace(args.Dest) != "" {
		rel, err := sharedRelPath(c.sharedDir, args.Dest)
		if err != nil {
			return mcpToolError("dest: " + err.Error()), nil
		}
		dest = rel
	}

	listing, err := c.drop.list(ctx, id, args.Password)
	if err != nil {
		logger.Info("drop listing failed", "tool", dropDownloadTool, "id", id, "err", err)
		return mcpToolError("받지 못했다: " + err.Error()), nil
	}
	picked, err := pickDropFiles(listing.Files, args.Files)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}

	root, err := os.OpenRoot(c.sharedDir)
	if err != nil {
		return mcpToolError("공유 볼륨을 열지 못했다: " + err.Error()), nil
	}
	defer root.Close()
	if err := mkdirAllIn(root, dest); err != nil {
		return mcpToolError(fmt.Sprintf("대상 디렉터리 %s 를 만들지 못했다(공유 볼륨 밖을 가리키거나 파일이 있다): %v", dest, err)), nil
	}
	destRoot, err := root.OpenRoot(dest)
	if err != nil {
		return mcpToolError("대상 디렉터리를 열지 못했다: " + err.Error()), nil
	}
	defer destRoot.Close()

	names := make([]string, len(picked))
	taken := map[string]bool{}
	for i, n := range picked {
		name := dropSafeName(listing.Files[n].Name, n)
		for taken[name] {
			name = strconv.Itoa(n) + "-" + name
		}
		taken[name] = true
		names[i] = name
		// 하나라도 이미 있으면 아무것도 받기 전에 끝낸다 — 절반만 받은 상태를 남기지 않는다.
		if _, err := destRoot.Lstat(name); err == nil && !args.Overwrite {
			return mcpToolError(fmt.Sprintf("%s 가 이미 있다 — 덮어쓰려면 overwrite:true, 아니면 다른 dest 를 준다",
				filepath.Join(c.sharedDir, dest, name))), nil
		}
	}

	written := make([]dropDownloadedFile, 0, len(picked))
	var total int64
	for i, n := range picked {
		want := listing.Files[n]
		size, err := c.drop.fetchInto(ctx, destRoot, listing.ID, args.Password, n, want.Size, names[i], args.Overwrite)
		if err != nil {
			logger.Error("drop download failed", "tool", dropDownloadTool, "id", id, "file", n, "err", err)
			return mcpToolError(fmt.Sprintf("%s 를 받지 못했다: %v (앞서 받은 %d 개는 남아 있다)", want.Name, err, len(written))), nil
		}
		written = append(written, dropDownloadedFile{Name: want.Name, Path: filepath.Join(c.sharedDir, dest, names[i]), Size: size})
		total += size
	}
	logger.Info("downloaded from drop", "tool", dropDownloadTool, "id", id, "files", len(written), "bytes", total)
	payload, err := json.Marshal(map[string]any{
		"id": listing.ID, "dest": filepath.Join(c.sharedDir, dest), "files": written, "bytes": total,
		"expires_at": listing.ExpiresAt,
	})
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	return mcpToolText(string(payload)), nil
}

// pickDropFiles 는 files 인자(이름 또는 0부터의 번호)를 목록의 번호로 바꾼다. 비면 전부다.
func pickDropFiles(files []dropFile, selectors []json.RawMessage) ([]int, error) {
	if len(selectors) == 0 {
		all := make([]int, len(files))
		for i := range files {
			all[i] = i
		}
		return all, nil
	}
	seen := map[int]bool{}
	var picked []int
	for _, raw := range selectors {
		var index float64
		var name string
		n := -1
		switch {
		case json.Unmarshal(raw, &index) == nil:
			if index != math.Trunc(index) || index < 0 || index >= float64(len(files)) {
				return nil, fmt.Errorf("files: 번호 %v 가 없다(0~%d)", index, len(files)-1)
			}
			n = int(index)
		case json.Unmarshal(raw, &name) == nil:
			for i, f := range files {
				if f.Name == name {
					n = i
					break
				}
			}
			if n < 0 {
				return nil, fmt.Errorf("files: 이름 %q 인 파일이 이 보관함에 없다", truncateText(name, 80))
			}
		default:
			return nil, errors.New("files 의 항목은 파일 이름(문자열)이나 번호(정수)다")
		}
		if !seen[n] {
			seen[n] = true
			picked = append(picked, n)
		}
	}
	return picked, nil
}

func dropGetError(err error) error {
	var apiErr *dropAPIError
	if errors.As(err, &apiErr) {
		switch apiErr.status {
		case http.StatusUnauthorized:
			// 어느 쪽이 틀렸는지 drop 도 말하지 않고 여기서도 말하지 않는다.
			return errors.New("아이디 또는 비밀번호가 맞지 않거나 보관 기간이 끝났다(401)")
		case http.StatusTooManyRequests:
			return errors.New("이 아이디에 틀린 시도가 너무 많다 — 15분 뒤에 다시 시도한다(429)")
		}
	}
	return err
}

func (c *dropClient) list(ctx context.Context, id, password string) (dropListing, error) {
	ctx, cancel := context.WithTimeout(ctx, dropRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+dropGetPath+url.PathEscape(id), nil)
	if err != nil {
		return dropListing{}, errors.New("drop 주소가 올바르지 않다")
	}
	req.SetBasicAuth(id, password)
	var out dropListing
	if err := c.do(req, http.StatusOK, &out); err != nil {
		return dropListing{}, dropGetError(err)
	}
	if out.ID == "" {
		out.ID = id
	}
	return out, nil
}

// fetchInto 는 n 번째 파일을 대상 디렉터리의 임시 파일로 흘려 받은 뒤 이름을 바꾼다. 쓰기는 전부
// destRoot 의 디렉터리 fd 기준이라 경로가 그 사이 바뀌어도 대상 디렉터리 밖으로 나가지 않고,
// 다른 이름으로 바꾸는 rename 은 링크를 따라가지 않고 그 항목 자체를 바꾼다.
func (c *dropClient) fetchInto(ctx context.Context, destRoot *os.Root, id, password string, n int, want int64, name string, overwrite bool) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+dropGetPath+url.PathEscape(id)+"/f/"+strconv.Itoa(n), nil)
	if err != nil {
		return 0, errors.New("drop 주소가 올바르지 않다")
	}
	req.SetBasicAuth(id, password)
	// 응답 헤더까지는 깨우기 대기를 포함한 메타데이터 마감을, 그 뒤 본문은 정체 감시를 건다.
	headerTimer := time.AfterFunc(dropRequestTimeout, cancel)
	resp, err := c.client.Do(req)
	headerTimer.Stop()
	if err != nil {
		return 0, errors.New("drop 에서 파일을 받지 못했다")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxDropJSONBytes))
		return 0, dropGetError(&dropAPIError{status: resp.StatusCode, message: dropErrorText(raw)})
	}

	tmpName := fmt.Sprintf(".drop-%d-%d.part", os.Getpid(), time.Now().UnixNano())
	tmp, err := destRoot.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("임시 파일을 만들지 못했다: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			destRoot.Remove(tmpName)
		}
	}()
	stall := time.AfterFunc(dropStallTimeout, cancel)
	defer stall.Stop()
	body := &stallReader{r: resp.Body, timer: stall, d: dropStallTimeout}
	// 목록이 말한 크기보다 1바이트 더 읽어 「더 길다」를 가려낸다.
	got, err := io.Copy(tmp, io.LimitReader(body, want+1))
	if err != nil {
		if ctx.Err() != nil {
			return 0, errors.New("drop 에서 받는 바이트가 멈췄다")
		}
		return 0, fmt.Errorf("받는 중에 끊겼다: %w", err)
	}
	if got != want {
		return 0, fmt.Errorf("받은 크기 %d 가 목록의 %d 와 다르다", got, want)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("파일을 닫지 못했다: %w", err)
	}
	if !overwrite {
		if _, err := destRoot.Lstat(name); err == nil {
			return 0, fmt.Errorf("%s 가 받는 사이에 생겼다 — 덮어쓰지 않았다", name)
		}
	}
	if err := renameIn(destRoot, tmpName, name); err != nil {
		return 0, fmt.Errorf("받은 파일을 제자리에 두지 못했다: %w", err)
	}
	committed = true
	return got, nil
}

// renameIn 은 같은 디렉터리 안에서 이름을 바꾼다(Go 1.24 의 Root 에는 Rename 이 없다).
func renameIn(root *os.Root, from, to string) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	fd := int(dir.Fd())
	return syscall.Renameat(fd, from, fd, to)
}

// stallReader 는 읽을 때마다 정체 타이머를 다시 건다.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(s.d)
	}
	return n, err
}

// dropToolDefinitions 는 drop 이 설정된 배포에서 tools/list 에 더해지는 두 도구다. 목록 자체는
// 게이트가 설 때만 나간다(R2 — toolDefinitions 가 먼저 끊는다).
func dropToolDefinitions() []any {
	return []any{
		map[string]any{
			"name": dropUploadTool,
			"description": "Upload files from this session's shared volume to drop, the operator's file-share service, " +
				"so a person can download them with an id and password. Give paths on the shared volume; this server " +
				"reads the files itself, so file contents never pass through this call or its result. Needs no " +
				"approval. The result carries only metadata: id, password, url, files, bytes, expires_at.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"paths": map[string]any{
						"type":        "array",
						"description": "Regular files on the shared volume, relative to it or absolute under its mount path. Directories are not accepted.",
						"items":       map[string]any{"type": "string"},
						"minItems":    1,
						"maxItems":    dropMaxFiles,
					},
					"days": map[string]any{
						"type":        "integer",
						"description": fmt.Sprintf("How many days drop keeps the files (1-%d, default %d).", dropMaxDays, dropDefaultDays),
						"minimum":     1,
						"maximum":     dropMaxDays,
					},
					"id": map[string]any{
						"type":        "string",
						"description": "Optional drop id: lowercase letters, digits and hyphens, 3-40 characters. Omit to let drop choose one.",
					},
					"password": map[string]any{
						"type":        "string",
						"description": "Optional download password, at least 6 characters. Omit to let drop generate one.",
					},
				},
				"required":             []any{"paths"},
				"additionalProperties": false,
			},
		},
		map[string]any{
			"name": dropDownloadTool,
			"description": "Download the files of a drop (by id and password) into a directory on this session's shared " +
				"volume. This server writes the files itself, so their contents never pass through this call or its " +
				"result; read them from the returned paths. Existing files are not overwritten unless overwrite is true. " +
				"Needs no approval.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "string", "description": "The drop id."},
					"password": map[string]any{"type": "string", "description": "The drop's password."},
					"files": map[string]any{
						"type":        "array",
						"description": "Optional subset to download: file names, or 0-based indexes in the drop's file list. Omit for all files.",
						"items":       map[string]any{"type": []any{"string", "integer"}},
						"maxItems":    dropMaxFiles,
					},
					"dest": map[string]any{
						"type":        "string",
						"description": "Directory on the shared volume to write into, relative to it or absolute under its mount path. Default: " + dropDownloadsRoot + "/<id>.",
					},
					"overwrite": map[string]any{
						"type":        "boolean",
						"description": "Replace files that already exist at the destination. Default false.",
					},
				},
				"required":             []any{"id", "password"},
				"additionalProperties": false,
			},
		},
	}
}
