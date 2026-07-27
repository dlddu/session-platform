package checkpointstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 records the requests the store makes and returns canned results, so the
// store's key/ref logic is exercised without real AWS.
type fakeS3 struct {
	putIn   *s3.PutObjectInput
	putBody []byte
	putErr  error

	getIn   *s3.GetObjectInput
	getBody string
	getErr  error
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putIn = in
	if in.Body != nil {
		f.putBody, _ = io.ReadAll(in.Body)
	}
	return &s3.PutObjectOutput{}, f.putErr
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getIn = in
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(f.getBody))}, nil
}

var _ objectAPI = (*fakeS3)(nil)

// Put joins the prefix with the key, streams the body verbatim, and returns the
// durable s3:// ref.
func TestS3_PutStoresUnderPrefixAndReturnsRef(t *testing.T) {
	fake := &fakeS3{}
	store := newWithAPI(fake, "ckpt-bucket", "checkpoints")

	ref, err := store.Put(context.Background(), "sessions/sess-abcd/checkpoint-1.tar", strings.NewReader("ARCHIVE-BYTES"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref != "s3://ckpt-bucket/checkpoints/sessions/sess-abcd/checkpoint-1.tar" {
		t.Errorf("ref = %q, want the prefixed s3:// URI", ref)
	}
	if fake.putIn == nil || *fake.putIn.Bucket != "ckpt-bucket" {
		t.Fatalf("PutObject bucket = %v, want ckpt-bucket", fake.putIn)
	}
	if got := *fake.putIn.Key; got != "checkpoints/sessions/sess-abcd/checkpoint-1.tar" {
		t.Errorf("PutObject key = %q, want the prefixed key", got)
	}
	if string(fake.putBody) != "ARCHIVE-BYTES" {
		t.Errorf("uploaded body = %q, want the archive bytes verbatim", fake.putBody)
	}
	if store.Bucket() != "ckpt-bucket" {
		t.Errorf("Bucket() = %q, want ckpt-bucket", store.Bucket())
	}
}

// An empty prefix falls back to the default so archives are never written to the
// bucket root.
func TestS3_PutDefaultsPrefix(t *testing.T) {
	fake := &fakeS3{}
	store := newWithAPI(fake, "b", "")
	if _, err := store.Put(context.Background(), "k.tar", strings.NewReader("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := *fake.putIn.Key; got != defaultPrefix+"/k.tar" {
		t.Errorf("key = %q, want default prefix %q", got, defaultPrefix)
	}
}

// A PutObject failure surfaces from Put.
func TestS3_PutSurfacesError(t *testing.T) {
	fake := &fakeS3{putErr: errors.New("access denied")}
	store := newWithAPI(fake, "b", "p")
	if _, err := store.Put(context.Background(), "k.tar", strings.NewReader("x")); err == nil {
		t.Fatal("put succeeded despite PutObject failure; want error")
	}
}

// Get parses the s3:// ref back into bucket/key and streams the object body.
func TestS3_GetParsesRefAndStreams(t *testing.T) {
	fake := &fakeS3{getBody: "RESTORED-ARCHIVE"}
	store := newWithAPI(fake, "b", "p")

	rc, err := store.Get(context.Background(), "s3://other-bucket/checkpoints/sessions/sess-x/c.tar")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "RESTORED-ARCHIVE" {
		t.Errorf("body = %q, want the object bytes", body)
	}
	// The ref, not the store's own bucket/prefix, drives the request.
	if got := *fake.getIn.Bucket; got != "other-bucket" {
		t.Errorf("GetObject bucket = %q, want other-bucket (from the ref)", got)
	}
	if got := *fake.getIn.Key; got != "checkpoints/sessions/sess-x/c.tar" {
		t.Errorf("GetObject key = %q, want the ref's key", got)
	}
}

// Get rejects refs that are not well-formed s3://bucket/key URIs.
func TestS3_GetRejectsBadRef(t *testing.T) {
	store := newWithAPI(&fakeS3{}, "b", "p")
	for _, ref := range []string{"", "not-s3", "s3://", "s3://bucket-only", "s3://bucket/"} {
		if _, err := store.Get(context.Background(), ref); err == nil {
			t.Errorf("Get(%q) succeeded; want error for a malformed ref", ref)
		}
	}
}

// NewS3 refuses incomplete config (so a half-set store never silently no-ops)
// and builds a store from complete config without any network call (credentials
// resolve lazily on first use).
func TestNewS3_ValidatesConfig(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []Config{
		{},
		{Bucket: "b"}, // missing region
		{Bucket: "b", RoleARN: "arn:aws:iam::123:role/r"}, // missing region
		{Region: "us-east-1"},                             // missing bucket
	} {
		if _, err := NewS3(ctx, cfg); err == nil {
			t.Errorf("NewS3(%+v) succeeded; want error for incomplete config", cfg)
		}
	}

	store, err := NewS3(ctx, Config{Bucket: "b", RoleARN: "arn:aws:iam::123:role/r", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewS3 with complete config: %v", err)
	}
	if store == nil || store.Bucket() != "b" {
		t.Fatalf("NewS3 returned %+v, want a store bound to bucket b", store)
	}
}

// A role ARN is optional: without one the ambient credentials are used directly
// (IRSA, or the static keys the S3-compatible e2e backend uses). An endpoint
// override targets an S3-compatible backend such as MinIO.
func TestNewS3_RoleOptionalAndEndpointOverride(t *testing.T) {
	ctx := context.Background()
	store, err := NewS3(ctx, Config{
		Bucket:   "checkpoints",
		Region:   "us-east-1",
		Endpoint: "http://minio:9000",
	})
	if err != nil {
		t.Fatalf("NewS3 without a role but with an endpoint: %v", err)
	}
	if store.Bucket() != "checkpoints" {
		t.Fatalf("bucket = %q, want checkpoints", store.Bucket())
	}
}
