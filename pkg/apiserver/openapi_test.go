package apiserver

import (
	"fmt"
	"testing"

	"jdix.io/sandbox/internal/openapicheck"
)

// specPath is the contract the SDKs are generated from.
const specPath = "../../api/openapi/control-plane.yaml"

func controlPlaneSpec(t *testing.T) openapicheck.Doc {
	return openapicheck.Load(t, specPath)
}

func TestControlPlaneSpecMatchesTheServer(t *testing.T) {
	srv := &Server{}
	served := make([]string, 0, len(srv.Routes()))
	for _, r := range srv.Routes() {
		served = append(served, fmt.Sprintf("%s %s", r.Method, r.Path))
	}
	openapicheck.SameSurface(t, controlPlaneSpec(t), served)
}

func TestControlPlaneOperationsAreNamed(t *testing.T) {
	openapicheck.Named(t, controlPlaneSpec(t))
}

func TestControlPlaneSuccessResponsesAreTyped(t *testing.T) {
	openapicheck.TypedSuccess(t, controlPlaneSpec(t))
}
