package checkpointstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AutoArchiveURLLifetime is how long a signed automatic-archive upload URL
// stays valid (AC-B5).
const AutoArchiveURLLifetime = MinSigningLifetime

var podNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

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

func autoArchiveKey(sessionID, pod string) (string, error) {
	if !session.ValidSessionID(sessionID) || !podNamePattern.MatchString(pod) {
		return "", fmt.Errorf("automatic archive key for session %q pod %q: malformed name", sessionID, pod)
	}
	return "auto/" + sessionID + "/" + pod + "/checkpoint.tar", nil
}

// AutoArchiveRef is the durable ref of the pod's automatic archive. Every
// upload from the same pod overwrites it.
func (s *S3) AutoArchiveRef(sessionID, pod string) (string, error) {
	key, err := autoArchiveKey(sessionID, pod)
	if err != nil {
		return "", err
	}
	return "s3://" + s.bucket + "/" + s.key(key), nil
}

// AutoArchiveTarget signs a URL that can upload the pod's automatic archive
// and nothing else, valid for AutoArchiveURLLifetime from the returned time.
func (s *S3) AutoArchiveTarget(ctx context.Context, sessionID, pod string) (string, time.Time, error) {
	if s.auto == nil {
		return "", time.Time{}, errors.New("automatic archive signing is not configured")
	}
	now := s.auto.now()
	if s.auto.creds != nil {
		creds, err := s.auto.creds.Retrieve(ctx)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("credentials for an automatic archive URL: %w", err)
		}
		// The cache reports Expires credentialsExpiryWindow early (store.go).
		if creds.CanExpire && creds.Expires.Add(credentialsExpiryWindow).Sub(now) < AutoArchiveURLLifetime {
			return "", time.Time{}, fmt.Errorf("credentials expire at %s, before a URL signed now would",
				creds.Expires.Add(credentialsExpiryWindow).UTC().Format(time.RFC3339))
		}
	}
	signed, err := s.signAutoArchive(ctx, sessionID, pod)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, now.Add(AutoArchiveURLLifetime), nil
}

// AutoArchiveBase is the pod's upload URL without its signature: the only
// destination the pod accepts a URL for.
func (s *S3) AutoArchiveBase(ctx context.Context, sessionID, pod string) (string, error) {
	if s.auto == nil {
		return "", errors.New("automatic archive signing is not configured")
	}
	signed, err := s.signAutoArchive(ctx, sessionID, pod)
	if err != nil {
		return "", err
	}
	return URLBase(signed)
}

func (s *S3) signAutoArchive(ctx context.Context, sessionID, pod string) (string, error) {
	key, err := autoArchiveKey(sessionID, pod)
	if err != nil {
		return "", err
	}
	req, err := s.auto.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	}, s3.WithPresignExpires(AutoArchiveURLLifetime))
	if err != nil {
		return "", fmt.Errorf("sign automatic archive upload for pod %s: %w", pod, err)
	}
	return req.URL, nil
}

// URLBase strips the query and fragment from raw.
func URLBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	return u.String(), nil
}

// Exists reports whether the object at ref is there.
func (s *S3) Exists(ctx context.Context, ref string) (bool, error) {
	bucket, key, err := parseRef(ref)
	if err != nil {
		return false, err
	}
	if _, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		if isMissingObject(err) {
			return false, nil
		}
		return false, fmt.Errorf("head %s: %w", ref, err)
	}
	return true, nil
}
