package apiserver

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// quotaError carries the retry hint alongside the refusal, so a client backs
// off correctly instead of hammering the endpoint.
type quotaError struct {
	Code              string
	Message           string
	RetryAfterSeconds int
}

func (e *quotaError) Error() string { return e.Code + ": " + e.Message }

// checkQuota enforces the tenant's limits.
//
// A tenant with no quota row is unlimited, which is right for a single-tenant
// install and wrong for a shared one — so provisioning must always write a row,
// and the Console shows tenants that have none.
func (s *Server) checkQuota(ctx context.Context, p *Principal) error {
	q, err := s.Store.Quota(ctx, p.TenantID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	if q.MaxConcurrent > 0 {
		running, err := s.Store.CountRunning(ctx, p.TenantID)
		if err != nil {
			return err
		}
		if running >= q.MaxConcurrent {
			return &quotaError{
				Code: "quota_concurrent",
				Message: fmt.Sprintf("this tenant already has %d of %d sandboxes running; delete one or wait for a TTL to elapse",
					running, q.MaxConcurrent),
				RetryAfterSeconds: 30,
			}
		}
	}

	if q.MaxCreatePerMinute > 0 {
		since := s.now().Add(-time.Minute)
		created, err := s.Store.CountCreatedSince(ctx, p.TenantID, since)
		if err != nil {
			return err
		}
		if created >= q.MaxCreatePerMinute {
			return &quotaError{
				Code: "quota_create_rate",
				Message: fmt.Sprintf("this tenant has created %d sandboxes in the last minute, at a limit of %d",
					created, q.MaxCreatePerMinute),
				RetryAfterSeconds: 10,
			}
		}
	}
	return nil
}
