package api

import (
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/e2b-dev/infra/packages/shared/pkg/stresstest"
)

// ConfigureStressSchema removes the old refresh endpoint's one-hour cap while
// preserving authentication, request types and all other schema validation.
func ConfigureStressSchema(spec *openapi3.T) {
	if !stresstest.Enabled() {
		return
	}
	refresh := spec.Paths.Value("/sandboxes/{sandboxID}/refreshes")
	refresh.Post.RequestBody.Value.Content["application/json"].Schema.Value.Properties["duration"].Value.Max = nil
}
