package store

import (
	"sort"
	"testing"
)

func TestMigrationNames_SortedAndFiltered(t *testing.T) {
	names, err := migrationNames()
	if err != nil {
		t.Fatalf("migrationNames: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("expected at least one embedded migration")
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("migrations must be applied in sorted order, got %v", names)
	}
	for _, n := range names {
		if len(n) == 0 || n[len(n)-7:] != ".up.sql" {
			t.Errorf("unexpected migration file name %q", n)
		}
	}
}
