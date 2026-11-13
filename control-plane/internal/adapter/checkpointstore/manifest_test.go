package checkpointstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const manifestID = "0123456789abcdef0123456789abcdef"

func TestS3_ManifestRoundTripsUnderItsOwnKey(t *testing.T) {
	fake := &fakeS3{}
	store := newWithAPI(fake, "b", "bronze/session-platform/checkpoints")
	want := session.Manifest{
		Version:      session.ManifestVersion,
		ID:           manifestID,
		Name:         "agent",
		WorkloadType: session.WorkloadTypeClaudeCode,
		Model:        "claude-opus-5-5",
		Tenant:       "pocket-aide",
		CreatedAt:    time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Checkpoint:   session.Checkpoint{Ref: "s3://b/bronze/session-platform/checkpoints/sess-x/g/checkpoint.tar", SizeBytes: 42},
	}
	if err := store.PutManifest(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if got := *fake.putIn.Key; got != "bronze/session-platform/checkpoints/manifests/"+manifestID+".json" {
		t.Fatalf("manifest key = %q", got)
	}
	fake.getBody = string(fake.putBody)
	got, err := store.GetManifest(context.Background(), manifestID)
	if err != nil {
		t.Fatal(err)
	}
	if *fake.getIn.Key != *fake.putIn.Key {
		t.Fatalf("read key %q, wrote %q", *fake.getIn.Key, *fake.putIn.Key)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("round trip = %s, want %s", b, a)
	}
}

func TestS3_ManifestMissingIsNotFoundButDeniedIsNot(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		notFound bool
	}{
		{"no such key", &types.NoSuchKey{}, true},
		{"bare 404", &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusNotFound}}, Err: errors.New("not found")}, true},
		{"access denied", &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusForbidden}}, Err: errors.New("denied")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newWithAPI(&fakeS3{getErr: tc.err}, "b", "p")
			_, err := store.GetManifest(context.Background(), manifestID)
			if got := errors.Is(err, session.ErrNotFound); got != tc.notFound || err == nil {
				t.Fatalf("err = %v, not-found = %v, want %v", err, got, tc.notFound)
			}
		})
	}
}

func TestS3_ManifestRefusesMalformedIDsAndLayouts(t *testing.T) {
	fake := &fakeS3{getBody: `{"version":2,"id":"` + manifestID + `","checkpoint":{"ref":"s3://b/k"}}`}
	store := newWithAPI(fake, "b", "p")
	if _, err := store.GetManifest(context.Background(), "../escape"); !errors.Is(err, session.ErrNotFound) || fake.getIn != nil {
		t.Fatalf("malformed id: err=%v, reached S3=%v", err, fake.getIn != nil)
	}
	if err := store.PutManifest(context.Background(), session.Manifest{ID: "../escape"}); err == nil || fake.putIn != nil {
		t.Fatalf("malformed put: err=%v, reached S3=%v", err, fake.putIn != nil)
	}
	if _, err := store.GetManifest(context.Background(), manifestID); err == nil {
		t.Fatal("an unknown manifest version must not decode")
	}
}
