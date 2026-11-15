package checkpointstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

// fakeS3 records the requests the store makes and returns canned results.
type fakeS3 struct {
	putIn   *s3.PutObjectInput
	putBody []byte
	putErr  error

	getIn     *s3.GetObjectInput
	getBody   string
	getWriter string
	getErr    error

	headIn     *s3.HeadObjectInput
	headErr    error
	headWriter string
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.headIn = in
	if f.headErr != nil {
		return nil, f.headErr
	}
	return &s3.HeadObjectOutput{Metadata: map[string]string{"writer": f.headWriter}}, nil
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
	return &s3.GetObjectOutput{
		Body:     io.NopCloser(strings.NewReader(f.getBody)),
		Metadata: map[string]string{"writer": f.getWriter},
	}, nil
}

var _ objectAPI = (*fakeS3)(nil)

func TestS3_PutStoresUnderPrefixAndReturnsRef(t *testing.T) {
	fake := &fakeS3{}
	store := newWithAPI(fake, "ckpt-bucket", "checkpoints")

	ref, err := store.Put(context.Background(), "sessions/sess-abcd/checkpoint-1.tar", "generation:g", strings.NewReader("ARCHIVE-BYTES"))
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
	if got := fake.putIn.Metadata["writer"]; got != "generation:g" {
		t.Errorf("writer metadata = %q, want generation:g", got)
	}
	if store.Bucket() != "ckpt-bucket" {
		t.Errorf("Bucket() = %q, want ckpt-bucket", store.Bucket())
	}
}

// unseekableReader exposes only Read, hiding the Seek/ReadAt that strings.Reader
// happens to provide — the shape of the real input, where the pod agent streams
// the archive as it dumps.
type unseekableReader struct{ r io.Reader }

func (u unseekableReader) Read(p []byte) (int, error) { return u.r.Read(p) }

// The trailing-checksum fallback for an unseekable body needs TLS — that failed
// against the e2e MinIO backend, so Put must spool and still carry exact bytes.
func TestS3_PutSpoolsUnseekableStream(t *testing.T) {
	fake := &fakeS3{}
	store := newWithAPI(fake, "b", "p")

	archive := strings.Repeat("FILESYSTEM-ARCHIVE-", 1000)
	ref, err := store.Put(context.Background(), "sess-abcd/checkpoint.tar", "generation:g",
		unseekableReader{r: strings.NewReader(archive)})
	if err != nil {
		t.Fatalf("put of an unseekable stream: %v", err)
	}
	if ref != "s3://b/p/sess-abcd/checkpoint.tar" {
		t.Errorf("ref = %q", ref)
	}
	if string(fake.putBody) != archive {
		t.Errorf("uploaded %d bytes, want the %d-byte archive verbatim", len(fake.putBody), len(archive))
	}
}

// An empty prefix falls back to the default so archives are never written to the
// bucket root.
func TestS3_PutDefaultsPrefix(t *testing.T) {
	fake := &fakeS3{}
	store := newWithAPI(fake, "b", "")
	if _, err := store.Put(context.Background(), "k.tar", "generation:g", strings.NewReader("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := *fake.putIn.Key; got != defaultPrefix+"/k.tar" {
		t.Errorf("key = %q, want default prefix %q", got, defaultPrefix)
	}
}

func TestS3_PutSurfacesError(t *testing.T) {
	fake := &fakeS3{putErr: errors.New("access denied")}
	store := newWithAPI(fake, "b", "p")
	if _, err := store.Put(context.Background(), "k.tar", "generation:g", strings.NewReader("x")); err == nil {
		t.Fatal("put succeeded despite PutObject failure; want error")
	}
}

func TestS3_GetParsesRefAndStreams(t *testing.T) {
	fake := &fakeS3{getBody: "RESTORED-ARCHIVE", getWriter: "pod:sess-x"}
	store := newWithAPI(fake, "b", "p")

	rc, writer, err := store.Get(context.Background(), "s3://other-bucket/checkpoints/sessions/sess-x/c.tar")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if writer != "pod:sess-x" {
		t.Errorf("writer = %q, want the object's writer metadata", writer)
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

func TestS3_GetRejectsBadRef(t *testing.T) {
	store := newWithAPI(&fakeS3{}, "b", "p")
	for _, ref := range []string{"", "not-s3", "s3://", "s3://bucket-only", "s3://bucket/"} {
		if _, _, err := store.Get(context.Background(), ref); err == nil {
			t.Errorf("Get(%q) succeeded; want error for a malformed ref", ref)
		}
	}
}

// A half-set config must not yield a store that silently no-ops.
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

type fakeAssumeRole struct {
	lifetime  time.Duration
	calls     int
	durations []int32
}

func (f *fakeAssumeRole) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.calls++
	f.durations = append(f.durations, aws.ToInt32(in.DurationSeconds))
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId:     aws.String("AKID"),
		SecretAccessKey: aws.String("SECRET"),
		SessionToken:    aws.String("TOKEN"),
		Expiration:      aws.Time(time.Now().Add(f.lifetime)),
	}}, nil
}

func TestRoleCredentials_AssumesForThreeHours(t *testing.T) {
	fake := &fakeAssumeRole{lifetime: roleSessionDuration}
	creds, err := roleCredentials(fake, "arn:aws:iam::123:role/r", "s").Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(fake.durations) != 1 || fake.durations[0] != int32((3*time.Hour).Seconds()) {
		t.Fatalf("DurationSeconds = %v, want [10800]", fake.durations)
	}
	if left := time.Until(creds.Expires) + credentialsExpiryWindow; left < MinSigningLifetime {
		t.Fatalf("handed-out credentials live %s, want at least %s", left, MinSigningLifetime)
	}
}

func TestRoleCredentials_ReusesWhileTwoHoursRemain(t *testing.T) {
	fake := &fakeAssumeRole{lifetime: roleSessionDuration}
	cache := roleCredentials(fake, "arn:aws:iam::123:role/r", "s")
	for i := 0; i < 3; i++ {
		if _, err := cache.Retrieve(context.Background()); err != nil {
			t.Fatalf("Retrieve #%d: %v", i, err)
		}
	}
	if fake.calls != 1 {
		t.Fatalf("AssumeRole calls = %d, want 1 while more than %s remains", fake.calls, MinSigningLifetime)
	}
}

func TestRoleCredentials_RefreshesBelowTwoHours(t *testing.T) {
	fake := &fakeAssumeRole{lifetime: MinSigningLifetime}
	cache := roleCredentials(fake, "arn:aws:iam::123:role/r", "s")
	for i := 0; i < 2; i++ {
		if _, err := cache.Retrieve(context.Background()); err != nil {
			t.Fatalf("Retrieve #%d: %v", i, err)
		}
	}
	if fake.calls != 2 {
		t.Fatalf("AssumeRole calls = %d, want 2: credentials with less than %s left must not be reused", fake.calls, credentialsExpiryWindow)
	}
}
