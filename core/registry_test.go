package core_test

import (
	"testing"

	"github.com/motherlodelab/magpie/core"
)

func TestRegistry_Gate(t *testing.T) {
	t.Cleanup(core.SaveRegistry())
	// Accept: same major, any minor/patch skew.
	for _, v := range []string{"1.0.0", "1.0.1", "1.9.0", "1.99.99"} {
		id := "test.accept." + v
		if err := core.RegisterModule(core.ModuleInfo{ID: id, APIVersion: v, Kind: core.KindFetcher}); err != nil {
			t.Errorf("RegisterModule(%s) = %v, want accept", v, err)
		}
	}
	// Reject: different major, empty, unparsable, empty ID.
	for _, tc := range []struct {
		name string
		id   string
		ver  string
	}{
		{"major-up", "test.reject.major-up", "2.0.0"},
		{"major-down", "test.reject.major-down", "0.9.9"},
		{"empty-version", "test.reject.empty", ""},
		{"not-semver", "test.reject.notsemver", "not-semver"},
		{"two-part", "test.reject.twopart", "1.0"},
		{"empty-id", "", "1.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := core.RegisterModule(core.ModuleInfo{ID: tc.id, APIVersion: tc.ver, Kind: core.KindFetcher}); err == nil {
				t.Errorf("RegisterModule(%q, %q) = nil, want reject", tc.id, tc.ver)
			}
		})
	}
}

func TestRegistry_Duplicate(t *testing.T) {
	t.Cleanup(core.SaveRegistry())
	id := "test.dup.module"
	if err := core.RegisterModule(core.ModuleInfo{ID: id, APIVersion: "1.0.0", Kind: core.KindExporter}); err != nil {
		t.Fatalf("first RegisterModule: %v", err)
	}
	if err := core.RegisterModule(core.ModuleInfo{ID: id, APIVersion: "1.0.0", Kind: core.KindExporter}); err == nil {
		t.Error("second RegisterModule = nil, want duplicate reject")
	}
}

func TestRegistry_MustRegisterPanics(t *testing.T) {
	t.Cleanup(core.SaveRegistry())
	id := "test.mustpanic.module"
	core.MustRegister(core.ModuleInfo{ID: id, APIVersion: "1.0.0", Kind: core.KindCleaner})
	defer func() {
		if recover() == nil {
			t.Error("MustRegister duplicate did not panic")
		}
	}()
	core.MustRegister(core.ModuleInfo{ID: id, APIVersion: "1.0.0", Kind: core.KindCleaner})
}

func TestRegistry_LookupMiss(t *testing.T) {
	if _, ok := core.Lookup("test.lookup.definitely-missing-xyz"); ok {
		t.Error("Lookup(missing) = true, want false")
	}
}

func TestRegistry_ListSortedAndCopy(t *testing.T) {
	t.Cleanup(core.SaveRegistry())
	for _, id := range []string{"test.list.b", "test.list.a"} { // registered out of order
		core.MustRegister(core.ModuleInfo{ID: id, APIVersion: "1.0.0", Kind: core.KindFetcher})
	}
	mods := core.ListModules()
	for i := 1; i < len(mods); i++ {
		if mods[i-1].ID >= mods[i].ID {
			t.Fatalf("ListModules not sorted: %q before %q", mods[i-1].ID, mods[i].ID)
		}
	}
	if len(mods) == 0 {
		t.Fatal("ListModules empty, want the registered test modules")
	}
	// Defensive copy: mutating the result must not affect the registry.
	mods[0].ID = "test.mutated"
	again := core.ListModules()
	for _, m := range again {
		if m.ID == "test.mutated" {
			t.Fatal("ListModules returned live slice (mutation leaked)")
		}
	}
}
