package engine

import (
	"reflect"
	"sync"
	"testing"
)

type testDriver struct{}

func (testDriver) LatexmkArgs() []string        { return []string{"-pdf", "-pdflatex=trusted-tex %O %S"} }
func (testDriver) GraphicsExtensions() []string { return []string{".custom"} }
func (testDriver) VersionProbe() Command {
	return Command{Name: "trusted-tex", Args: []string{"--version"}}
}

func TestRegistryUsesOpaqueNamesAndRejectsReplacement(t *testing.T) {
	var registry Registry
	name := "lab/custom TeX+2026"
	if err := registry.Register(name, testDriver{}); err != nil {
		t.Fatal(err)
	}
	driver, err := registry.Lookup(name)
	if err != nil || driver.VersionProbe().Name != "trusted-tex" {
		t.Fatalf("lookup: %v %v", driver, err)
	}
	if err := registry.Register(name, testDriver{}); err == nil {
		t.Fatal("replaced a registered engine")
	}
	if _, err := registry.Lookup("unknown-engine"); err == nil {
		t.Fatal("unknown engine used a fallback")
	}
	if _, err := registry.Lookup("Lab/custom TeX+2026"); err == nil {
		t.Fatal("engine names were silently normalized")
	}
	for _, name := range []string{"", " ", "bad\nname", "bad\x00name"} {
		if err := registry.Register(name, testDriver{}); err == nil {
			t.Fatalf("registered invalid name %q", name)
		}
	}
	if err := registry.Register("nil", nil); err == nil {
		t.Fatal("registered a missing driver")
	}
}

func TestRegistryConcurrentRegistrationAndSortedNames(t *testing.T) {
	registry := NewRegistry()
	var group sync.WaitGroup
	for _, name := range []string{"z-engine", "a-engine", "m-engine"} {
		group.Go(func() {
			if err := registry.Register(name, testDriver{}); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if want := []string{"a-engine", "m-engine", "z-engine"}; !reflect.DeepEqual(registry.Names(), want) {
		t.Fatal(registry.Names())
	}
	names := registry.Names()
	names[0] = "changed"
	if registry.Names()[0] != "a-engine" {
		t.Fatal("caller mutated registry names")
	}
}

func TestBuiltinDriverSlicesAreIndependentAndLuaIsHardened(t *testing.T) {
	driver, err := Default.Lookup("lualatex")
	if err != nil {
		t.Fatal(err)
	}
	args := driver.LatexmkArgs()
	if !reflect.DeepEqual(args, []string{"-lualatex", "-pdflualatex=lualatex --safer --nosocket %O %S"}) {
		t.Fatal(args)
	}
	args[0] = "changed"
	extensions := driver.GraphicsExtensions()
	extensions[0] = "changed"
	probe := driver.VersionProbe()
	probe.Args[0] = "changed"
	if driver.LatexmkArgs()[0] != "-lualatex" || driver.GraphicsExtensions()[0] != ".pdf" ||
		driver.VersionProbe().Args[0] != "--version" {
		t.Fatal("caller mutated a shared driver definition")
	}
}
