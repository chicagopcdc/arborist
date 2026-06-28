package arborist

import (
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestAuthMappingProjectExclusion(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		t.Setenv("AUTH_MAPPING_PROJECT_EXCLUSION", "")
		assert.Equal(t, "ARRAY[]::text[]", authMappingProjectExclusion())
	})

	t.Run("uses env value as-is", func(t *testing.T) {
		t.Setenv(
			"AUTH_MAPPING_PROJECT_EXCLUSION",
			"ARRAY['programs.pcdc.projects.20260113.%', 'programs.pcdc.projects.20251014.%']",
		)
		assert.Equal(
			t,
			"ARRAY['programs.pcdc.projects.20260113.%', 'programs.pcdc.projects.20251014.%']",
			authMappingProjectExclusion(),
		)
	})
}
