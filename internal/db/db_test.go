package db

import (
	"strings"
	"testing"
)

func TestMigrationContainsIndividualModel(t *testing.T) {
	content, err := migrations.ReadFile("migrations/001_core.sql")
	if err != nil || !strings.Contains(string(content), "UNIQUE(contest_id,user_id)") {
		t.Fatal("individual participant constraint missing")
	}
}
