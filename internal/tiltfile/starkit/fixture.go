package starkit

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
)

// A fixture for test setup/teardown
type Fixture struct {
	tb         testing.TB
	extensions []Extension
	path       string
}

func NewFixture(tb testing.TB, extensions ...Extension) *Fixture {
	dir, err := ioutil.TempDir("", tb.Name())
	if err != nil {
		tb.Fatalf("Creating TempDir: %v", err)
	}

	return &Fixture{
		tb:         tb,
		extensions: extensions,
		path:       dir,
	}
}

func (f *Fixture) ExecFile(name string) error {
	return ExecFile(filepath.Join(f.path, name), f.extensions...)
}

func (f *Fixture) Path() string {
	return f.path
}

func (f *Fixture) File(name, contents string) {
	fullPath := filepath.Join(f.path, name)
	err := os.MkdirAll(filepath.Dir(fullPath), os.FileMode(0777))
	if err != nil {
		f.tb.Fatalf("MkdirAll: %v", err)
	}

	err = ioutil.WriteFile(fullPath, []byte(contents), os.FileMode(0666))
	if err != nil {
		f.tb.Fatalf("WriteFile: %v", err)
	}
}

func (f *Fixture) TearDown() {
	_ = os.RemoveAll(f.path)
}
