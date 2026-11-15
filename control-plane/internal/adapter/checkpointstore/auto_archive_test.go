package checkpointstore

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const autoSession = "0123456789abcdef0123456789abcdef"

func TestArchiveRefIsOnePerSession(t *testing.T) {
	store := newWithAPI(&fakeS3{}, "b", "p")
	ref, err := store.ArchiveRef(autoSession)
	if err != nil {
		t.Fatal(err)
	}
	if want := "s3://b/p/sessions/" + autoSession + "/checkpoint.tar"; ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
	if _, err := store.ArchiveRef("not-a-session"); err == nil {
		t.Error("ArchiveRef accepted a malformed session id")
	}
}

func responseError(status int) error {
	return &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}}, Err: errors.New("x")}
}

func TestArchiveWriterTellsMissingFromForbidden(t *testing.T) {
	ref := "s3://b/p/sessions/" + autoSession + "/checkpoint.tar"
	for _, tc := range []struct {
		name       string
		headErr    error
		wantWriter string
		wantExists bool
		wantErr    bool
	}{
		{"present", nil, "pod:sess-x", true, false},
		{"missing", responseError(http.StatusNotFound), "", false, false},
		{"forbidden", responseError(http.StatusForbidden), "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeS3{headErr: tc.headErr, headWriter: "pod:sess-x"}
			writer, exists, err := newWithAPI(fake, "b", "p").ArchiveWriter(context.Background(), ref)
			if writer != tc.wantWriter || exists != tc.wantExists || (err != nil) != tc.wantErr {
				t.Fatalf("ArchiveWriter = %q, %v, %v; want %q, %v, error %v", writer, exists, err, tc.wantWriter, tc.wantExists, tc.wantErr)
			}
			if aws.ToString(fake.headIn.Key) != "p/sessions/"+autoSession+"/checkpoint.tar" {
				t.Fatalf("head key = %q", aws.ToString(fake.headIn.Key))
			}
		})
	}
}

type fixedCreds struct{ expires time.Time }

func (c fixedCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "A", SecretAccessKey: "S", CanExpire: true, Expires: c.expires}, nil
}

type fakePresign struct{ key string }

func (f *fakePresign) PresignPutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	f.key = aws.ToString(in.Key)
	return &v4.PresignedHTTPRequest{
		URL:          "https://b.s3/" + f.key + "?X-Amz-Signature=sig",
		SignedHeader: http.Header{"Host": {"b.s3"}, "X-Amz-Meta-Writer": {in.Metadata["writer"]}},
	}, nil
}

// The URL must not outlive the credentials that signed it. The cache's
// Expires is credentialsExpiryWindow earlier than the real expiry.
func TestAutoArchiveTargetRefusesShortLivedCredentials(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		realLeft  time.Duration
		wantError bool
	}{
		{"three hours left", 3 * time.Hour, false},
		{"two hours left", 2 * time.Hour, false},
		{"just under two hours", 2*time.Hour - time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			presign := &fakePresign{}
			store := newWithAPI(&fakeS3{}, "b", "p")
			store.auto = &autoArchiveSigner{
				presign: presign,
				creds:   fixedCreds{expires: now.Add(tc.realLeft - credentialsExpiryWindow)},
				now:     func() time.Time { return now },
			}
			got, expires, headers, err := store.AutoArchiveTarget(context.Background(), autoSession, "pod")
			if (err != nil) != tc.wantError {
				t.Fatalf("AutoArchiveTarget err = %v, want error %v", err, tc.wantError)
			}
			if err != nil {
				return
			}
			if !strings.Contains(got, "/p/sessions/"+autoSession+"/checkpoint.tar?") || !expires.Equal(now.Add(2*time.Hour)) {
				t.Fatalf("target = %q until %s", got, expires)
			}
			if len(headers) != 1 || headers["X-Amz-Meta-Writer"] != "pod:pod" {
				t.Fatalf("headers = %v, want only the signed writer", headers)
			}
		})
	}
}

// A real signature: path-style against an S3-compatible endpoint, valid for
// two hours, and with no checksum the archive upload could not match.
func TestAutoArchiveURLIsSignedForTwoHoursWithoutChecksum(t *testing.T) {
	base := aws.Config{
		Region:      "ap-northeast-2",
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
	}
	store := newWithAPI(&fakeS3{}, "bucket", "prefix")
	store.auto = newAutoArchiveSigner(base, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://minio:9000")
		o.UsePathStyle = true
	})
	signed, _, headers, err := store.AutoArchiveTarget(context.Background(), autoSession, "pod")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if headers["X-Amz-Meta-Writer"] != "pod:pod" {
		t.Fatalf("headers = %v, want the writer the signature covers", headers)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q0(u), "x-amz-meta-writer") {
		t.Fatalf("signed headers = %q, want the writer among them", q0(u))
	}
	if u.Path != "/bucket/prefix/sessions/"+autoSession+"/checkpoint.tar" {
		t.Fatalf("path = %q", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "7200" {
		t.Fatalf("X-Amz-Expires = %q, want 7200", q.Get("X-Amz-Expires"))
	}
	for key := range q {
		if strings.Contains(strings.ToLower(key), "checksum") {
			t.Fatalf("signed URL carries %s; the archive body would not match it", key)
		}
	}
	if got, err := store.AutoArchiveBase(context.Background(), autoSession, "pod"); err != nil || got != "http://minio:9000"+u.Path {
		t.Fatalf("base = %q, %v", got, err)
	}
}

func q0(u *url.URL) string { return u.Query().Get("X-Amz-SignedHeaders") }
