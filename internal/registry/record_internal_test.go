package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/files"
)

// fillByKind sets every field of value to something non-zero, by its kind, so
// a round trip that drops any of them shows. A kind it does not know how to
// fill is an error saying what to do, not a panic and not a skipped field: a
// new field must be taught to this guard, to recordFor and to
// parseRecordJSON together.
func fillByKind(value reflect.Value, path string) error {
	unknown := func() error {
		return fmt.Errorf("RegisteredWorktree%s has kind %s (%s), which this guard does not know how to fill; teach fillByKind, recordFor and parseRecordJSON about it",
			path, value.Kind(), value.Type())
	}
	if !value.CanSet() {
		return fmt.Errorf("RegisteredWorktree%s cannot be set (an unexported field?); export it, or teach fillByKind, recordFor and parseRecordJSON about it", path)
	}
	switch value.Kind() {
	case reflect.String:
		value.SetString("x")
	case reflect.Int:
		value.SetInt(7)
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Ptr:
		if value.Type().Elem().Kind() != reflect.String {
			return unknown()
		}
		text := "dev.example"
		value.Set(reflect.ValueOf(&text))
	case reflect.Map:
		if value.Type() != reflect.TypeOf(map[string]int{}) {
			return unknown()
		}
		value.Set(reflect.ValueOf(map[string]int{"api": 30000}))
	case reflect.Struct:
		for i := range value.NumField() {
			if err := fillByKind(value.Field(i), path+"."+value.Type().Field(i).Name); err != nil {
				return err
			}
		}
	default:
		return unknown()
	}
	return nil
}

// TestRecordCarriesEveryField fills every field of RegisteredWorktree by
// kind, writes the record with recordFor, reads it back through the real
// read paths (Store.readStoredRecord, then the Registry's registration()) and
// compares the whole struct: a field added to the struct and left out of
// recordFor, parseRecordJSON or a read path fails here, which is how a
// writer that does not know a field can never erase it (#119).
func TestRecordCarriesEveryField(t *testing.T) {
	filled := RegisteredWorktree{}
	if err := fillByKind(reflect.ValueOf(&filled).Elem(), ""); err != nil {
		t.Fatal(err)
	}
	filled.RepositoryID = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	filled.WorktreeID = "2b41d9d4-ed88-49c1-be9c-5439bdf6a431"
	filled.Slug = "slug"
	filled.Domain = "acme.localhost"

	store := NewStore(t.TempDir(), filled.RepositoryID)
	file := store.RecordPath(filled.WorktreeID)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := files.AtomicJSON(file, recordFor(filled, Complete)); err != nil {
		t.Fatal(err)
	}

	stored, err := store.readStoredRecord(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Registration, filled) || stored.Initialization != Complete {
		t.Fatalf("Store read path lost a field:\n got  %+v\n want %+v", stored.Registration, filled)
	}

	identity := WorktreeIdentity{
		RepositoryID: filled.RepositoryID, WorktreeID: filled.WorktreeID, Root: "/work/x", Branch: "main",
	}
	viaRegistry := registration(identity, stored.Registration)
	if !reflect.DeepEqual(viaRegistry.RegisteredWorktree, filled) || viaRegistry.Root != "/work/x" || viaRegistry.Branch != "main" {
		t.Fatalf("Registry read path lost a field:\n got  %+v\n want %+v", viaRegistry, filled)
	}
}

// TestFillByKindRefusesWhatItCannotFill proves the guard answers a pointer to
// a non-string, or a slice, with its guiding message and no panic.
func TestFillByKindRefusesWhatItCannotFill(t *testing.T) {
	type future struct {
		Routes *int
		Other  []string
	}
	value := reflect.ValueOf(&future{}).Elem()
	err := fillByKind(value, "")
	if err == nil || !strings.Contains(err.Error(), "teach fillByKind, recordFor and parseRecordJSON") {
		t.Fatalf("err = %v", err)
	}
	if err := fillByKind(value.Field(1), ".Other"); err == nil {
		t.Fatal("fillByKind accepted a slice")
	}
	type hidden struct {
		secret string //lint:ignore U1000 reflection is the point
	}
	err = fillByKind(reflect.ValueOf(&hidden{}).Elem(), "")
	if err == nil || !strings.Contains(err.Error(), "cannot be set") {
		t.Fatalf("an unexported field: err = %v", err)
	}
}
