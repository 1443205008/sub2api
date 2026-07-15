package migrations

import (
	"strings"
	"testing"
)

func TestGroupRateTimeRulesMigrationIsEmbeddedAndIdempotent(t *testing.T) {
	content, err := FS.ReadFile("177_add_group_rate_time_rules.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"ADD COLUMN IF NOT EXISTS rate_time_rules JSONB",
		"DEFAULT '[]'::jsonb",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}
}
