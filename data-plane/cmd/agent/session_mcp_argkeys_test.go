package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestArgumentKeys(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{``, []string{}},
		{`null`, []string{}},
		{`{}`, []string{}},
		{`{"paths":["a"],"password":"s3cret","id":"x","days":1}`, []string{"days", "id", "password", "paths"}},
		{`["a"]`, []string{"<not an object>"}},
	}
	for _, c := range cases {
		if got := argumentKeys(json.RawMessage(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("argumentKeys(%s) = %v, want %v", c.in, got, c.want)
		}
	}
}

// 도구 호출 로그에는 인자 이름만 남고 값(비밀번호 등)은 남지 않는다.
func TestCallToolLogsArgumentKeysNotValues(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	params := json.RawMessage(`{"name":"drop_upload","arguments":{"paths":["x"],"id":"sp-check","password":"very-secret-pw","days":1}}`)
	_, _ = sessionMCPConfig{}.callTool(context.Background(), logger, params)
	out := buf.String()
	if !strings.Contains(out, `"msg":"tool call received"`) || !strings.Contains(out, `"argument_keys":["days","id","password","paths"]`) {
		t.Fatalf("missing argument_keys log: %s", out)
	}
	if strings.Contains(out, "very-secret-pw") || strings.Contains(out, "sp-check") {
		t.Fatalf("argument values leaked into the log: %s", out)
	}
}
