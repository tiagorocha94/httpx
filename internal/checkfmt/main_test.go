package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUnformattedFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"formatted.go":          "package example\n",
		"needs_formatting.go":   "package example\nfunc f( ){}\n",
		"vendor/third_party.go": "package thirdparty\nfunc f( ){}\n",
	}
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := unformattedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"needs_formatting.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unformattedFiles() = %v, want %v", got, want)
	}
}
