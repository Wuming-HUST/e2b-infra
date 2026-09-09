package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStressRefreshSchema(t *testing.T) {
	t.Setenv("E2B_UNRESTRICTED_STRESS", "true")
	spec, err := GetSwagger()
	require.NoError(t, err)
	op := spec.Paths.Value("/sandboxes/{sandboxID}/refreshes").Post
	require.NotNil(t, op.RequestBody.Value.Content["application/json"].Schema.Value.Properties["duration"].Value.Max)
	ConfigureStressSchema(spec)
	require.Nil(t, op.RequestBody.Value.Content["application/json"].Schema.Value.Properties["duration"].Value.Max)
	require.NotEmpty(t, op.Security)
}
