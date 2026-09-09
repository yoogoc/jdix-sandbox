package initd

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"jdix.io/sandbox/internal/openapicheck"
)

const dataPlaneSpec = "../../api/openapi/data-plane.yaml"

// execdServedRoutes are part of the data plane but answered by execd before the
// request is forwarded here, so they are not in this package's route table.
// Listing them keeps the comparison honest rather than loosening it.
var execdServedRoutes = []string{
	"POST /v1/keepalive",
}

func dataPlaneDoc(t *testing.T) openapicheck.Doc {
	return openapicheck.Load(t, dataPlaneSpec)
}

func TestDataPlaneSpecMatchesTheServer(t *testing.T) {
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "/workspace")
	served := append([]string{}, execdServedRoutes...)
	for _, r := range srv.Routes() {
		served = append(served, fmt.Sprintf("%s %s", r.Method, r.Path))
	}
	openapicheck.SameSurface(t, dataPlaneDoc(t), served)
}

func TestDataPlaneOperationsAreNamed(t *testing.T) {
	openapicheck.Named(t, dataPlaneDoc(t))
}

func TestDataPlaneSuccessResponsesAreTyped(t *testing.T) {
	openapicheck.TypedSuccess(t, dataPlaneDoc(t))
}
