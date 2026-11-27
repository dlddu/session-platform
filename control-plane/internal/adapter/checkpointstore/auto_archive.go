package checkpointstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const AutoArchiveURLLifetime = MinSigningLifetime

var podNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

const writerMetadataKey = "writer"

func writerMetadata(writer string) map[string]string {
	if writer == "" {
		return nil
	}
	return map[string]string{writerMetadataKey: writer}
}

type presignAPI interface {
	PresignPutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

type autoArchiveSigner struct {
	presign presignAPI
	creds   aws.CredentialsProvider
	now     func() time.Time
}

func newAutoArchiveSigner(base aws.Config, endpoint func(*s3.Options)) *autoArchiveSigner {
	client := s3.NewFromConfig(base, endpoint, func(o *s3.Options) {
		// Signed in, a checksum would cover the empty request body here rather
		// than the archive the pod uploads later.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &autoArchiveSigner{presign: s3.NewPresignClient(client), creds: base.Credentials, now: time.Now}
}

func archiveKey(sessionID string) (string, error) {
	if !session.ValidSessionID(sessionID) {
		return "", fmt.Errorf("archive key for malformed session id %q", sessionID)
	}
	return session.ArchiveKey(sessionID), nil
}

// ArchiveRef is the durable ref of the session's archive, the one key its
// snapshots and its pods' automatic archives overwrite.
func (s *S3) ArchiveRef(sessionID string) (string, error) {
	key, err := archiveKey(sessionID)
	if err != nil {
		return "", err
	}
	return "s3://" + s.bucket + "/" + s.key(key), nil
}

// AutoArchiveTarget signs a URL that can upload pod's automatic archive to
// the session's archive key and nothing else, valid for
// AutoArchiveURLLifetime from the returned time. The upload must carry
// headers, which name pod as the writer.
func (s *S3) AutoArchiveTarget(ctx context.Context, sessionID, pod string) (string, time.Time, map[string]string, error) {
	if s.auto == nil {
		return "", time.Time{}, nil, errors.New("automatic archive signing is not configured")
	}
	now := s.auto.now()
	if s.auto.creds != nil {
		creds, err := s.auto.creds.Retrieve(ctx)
		if err != nil {
			return "", time.Time{}, nil, fmt.Errorf("credentials for an automatic archive URL: %w", err)
		}
		// The cache reports Expires credentialsExpiryWindow early (store.go).
		if creds.CanExpire && creds.Expires.Add(credentialsExpiryWindow).Sub(now) < AutoArchiveURLLifetime {
			return "", time.Time{}, nil, fmt.Errorf("credentials expire at %s, before a URL signed now would",
				creds.Expires.Add(credentialsExpiryWindow).UTC().Format(time.RFC3339))
		}
	}
	req, err := s.signAutoArchive(ctx, sessionID, pod)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	headers := map[string]string{}
	for name, values := range req.SignedHeader {
		if strings.EqualFold(name, "host") || len(values) == 0 {
			continue
		}
		headers[name] = values[0]
	}
	return req.URL, now.Add(AutoArchiveURLLifetime), headers, nil
}

// AutoArchiveBase is the session's archive URL without a signature: the only
// destination its pods accept a URL for.
func (s *S3) AutoArchiveBase(ctx context.Context, sessionID, pod string) (string, error) {
	if s.auto == nil {
		return "", errors.New("automatic archive signing is not configured")
	}
	req, err := s.signAutoArchive(ctx, sessionID, pod)
	if err != nil {
		return "", err
	}
	return URLBase(req.URL)
}

func (s *S3) signAutoArchive(ctx context.Context, sessionID, pod string) (*v4.PresignedHTTPRequest, error) {
	key, err := archiveKey(sessionID)
	if err != nil {
		return nil, err
	}
	if !podNamePattern.MatchString(pod) {
		return nil, fmt.Errorf("automatic archive for malformed pod name %q", pod)
	}
	req, err := s.auto.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.key(key)),
		Metadata: writerMetadata(session.PodWriter(pod)),
	}, s3.WithPresignExpires(AutoArchiveURLLifetime))
	if err != nil {
		return nil, fmt.Errorf("sign automatic archive upload for pod %s: %w", pod, err)
	}
	return req, nil
}

func URLBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	return u.String(), nil
}

func (s *S3) ArchiveWriter(ctx context.Context, ref string) (string, bool, error) {
	bucket, key, err := parseRef(ref)
	if err != nil {
		return "", false, err
	}
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isMissingObject(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("head %s: %w", ref, err)
	}
	return out.Metadata[writerMetadataKey], true, nil
}
