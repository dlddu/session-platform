package checkpointstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const maxManifestBytes = 1 << 20

func manifestKey(id string) string {
	return "manifests/" + id + ".json"
}

// PutManifest overwrites the session's manifest, so it always names the
// archive of its latest snapshot (AC-B4).
func (s *S3) PutManifest(ctx context.Context, m session.Manifest) error {
	if !session.ValidSessionID(m.ID) {
		return fmt.Errorf("manifest for malformed session id %q", m.ID)
	}
	wire, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode session manifest: %w", err)
	}
	full := s.key(manifestKey(m.ID))
	if _, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(full),
		Body:        bytes.NewReader(wire),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", s.bucket, full, err)
	}
	return nil
}

// GetManifest reads the session's manifest, or session.ErrNotFound when the
// id never committed a snapshot.
func (s *S3) GetManifest(ctx context.Context, id string) (*session.Manifest, error) {
	if !session.ValidSessionID(id) {
		return nil, session.ErrNotFound
	}
	full := s.key(manifestKey(id))
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(full),
	})
	if err != nil {
		if isMissingObject(err) {
			return nil, fmt.Errorf("%w: no manifest for session %s", session.ErrNotFound, id)
		}
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, full, err)
	}
	defer out.Body.Close()
	wire, err := io.ReadAll(io.LimitReader(out.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read s3://%s/%s: %w", s.bucket, full, err)
	}
	if len(wire) > maxManifestBytes {
		return nil, fmt.Errorf("s3://%s/%s exceeds %d bytes", s.bucket, full, maxManifestBytes)
	}
	return session.DecodeManifest(wire)
}

// isMissingObject recognises a missing key. S3 answers 404 only to a caller
// allowed to list the prefix; without that it answers 403 for a missing key
// too, which stays an error here rather than passing for "not found".
func isMissingObject(err error) bool {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var resp *smithyhttp.ResponseError
	return errors.As(err, &resp) && resp.HTTPStatusCode() == http.StatusNotFound
}
