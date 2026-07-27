// Package checkpointstore is the durable object store for CRIU checkpoint
// archives. The kubelet writes a checkpoint as a node-local tar; this package
// uploads it to S3 so the archive outlives the node and is reachable when the
// session restores onto a different node (decision ③, docs/criu-verification.md).
//
// Access is by STS AssumeRole layered over the ambient credential chain: the
// node instance profile (or IRSA) supplies the base credentials and this code
// assumes the configured role on top — no static keys in the control plane.
package checkpointstore

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// defaultSessionName is the STS role session name when none is configured. It
// tags the assumed-role sessions the control plane opens (visible in CloudTrail).
const defaultSessionName = "session-platform-checkpointer"

// defaultPrefix is the key prefix under which archives are stored.
const defaultPrefix = "checkpoints"

// Config configures the S3 checkpoint store. Bucket and Region are required;
// the rest are optional.
type Config struct {
	Bucket  string // target S3 bucket (required)
	Region  string // AWS region (required)
	RoleARN string // IAM role to assume for bucket access; empty uses the
	// ambient credentials directly (instance profile / IRSA / static env keys —
	// the last is how the S3-compatible e2e backend authenticates).
	Prefix      string // key prefix for archives (default "checkpoints")
	SessionName string // STS role session name (default defaultSessionName)
	Endpoint    string // S3-compatible endpoint (e.g. MinIO); empty = AWS S3
}

// objectAPI is the minimal S3 surface S3 uses. *s3.Client satisfies it; tests
// inject a fake so the store logic runs without real AWS.
type objectAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// S3 stores and retrieves checkpoint archives in an S3 bucket. Put satisfies the
// criu.CheckpointStore contract used by the checkpoint path; Get is the read
// half the restore path (node-side fetch / runc-restore alternative) uses.
type S3 struct {
	api    objectAPI
	bucket string
	prefix string
}

// NewS3 builds an S3 store whose client authenticates by assuming Config.RoleARN
// via STS, layered over the ambient credential chain (the node instance profile
// or IRSA provides the base credentials). Building the client makes no network
// call — credentials are resolved lazily on the first S3 request.
func NewS3(ctx context.Context, cfg Config) (*S3, error) {
	if cfg.Bucket == "" || cfg.Region == "" {
		return nil, fmt.Errorf("checkpoint S3 store needs bucket and region (got bucket=%q region=%q)",
			cfg.Bucket, cfg.Region)
	}

	// Base credentials come from the default chain — on a node that is the EC2
	// instance profile (via IMDS), IRSA when the pod has a projected token, or
	// static keys from the environment.
	base, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	if cfg.RoleARN != "" {
		// Assume the target role on top of the base credentials (STS AssumeRole),
		// caching the temporary credentials so they are reused until they near
		// expiry. Skipped when no role is configured — then the ambient
		// credentials are used as-is.
		sessionName := cfg.SessionName
		if sessionName == "" {
			sessionName = defaultSessionName
		}
		provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), cfg.RoleARN,
			func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = sessionName })
		base.Credentials = aws.NewCredentialsCache(provider)
	}

	client := s3.NewFromConfig(base, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			// An S3-compatible backend (MinIO in the e2e SUT): address the bucket
			// as a path rather than a virtual host, since these endpoints have no
			// per-bucket DNS.
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	})
	return newWithAPI(client, cfg.Bucket, cfg.Prefix), nil
}

// newWithAPI builds an S3 store over an injected object API (tests) and
// normalises the key prefix.
func newWithAPI(api objectAPI, bucket, prefix string) *S3 {
	p := strings.Trim(prefix, "/")
	if p == "" {
		p = defaultPrefix
	}
	return &S3{api: api, bucket: bucket, prefix: p}
}

// Bucket reports the bucket the store writes to (for logging).
func (s *S3) Bucket() string { return s.bucket }

// Put uploads the archive read from r under key (joined with the configured
// prefix) and returns its durable s3:// ref. r should be seekable (a file) so
// the SDK streams it without buffering the whole archive in memory.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) (string, error) {
	full := s.key(key)
	if _, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(full),
		Body:   r,
	}); err != nil {
		return "", fmt.Errorf("put s3://%s/%s: %w", s.bucket, full, err)
	}
	return "s3://" + s.bucket + "/" + full, nil
}

// Get opens the archive at ref (an s3:// URI) for reading. The caller closes the
// returned reader.
func (s *S3) Get(ctx context.Context, ref string) (io.ReadCloser, error) {
	bucket, key, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", ref, err)
	}
	return out.Body, nil
}

// key joins the configured prefix with a per-archive key.
func (s *S3) key(key string) string {
	return s.prefix + "/" + strings.TrimPrefix(key, "/")
}

// parseRef splits an s3://bucket/key URI into its parts.
func parseRef(ref string) (bucket, key string, err error) {
	const scheme = "s3://"
	if !strings.HasPrefix(ref, scheme) {
		return "", "", fmt.Errorf("not an s3 ref: %q", ref)
	}
	rest := ref[len(scheme):]
	i := strings.IndexByte(rest, '/')
	if i <= 0 || i >= len(rest)-1 {
		return "", "", fmt.Errorf("malformed s3 ref (want s3://bucket/key): %q", ref)
	}
	return rest[:i], rest[i+1:], nil
}
