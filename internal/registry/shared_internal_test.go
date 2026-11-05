package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
)

// fillSharedByKind sets every field of value to something non-zero, by its
// kind, so a round trip that drops any of them shows; fillByKind's
// counterpart for SharedRecord. A kind it does not know is an error saying
// what to teach, not a skipped field.
func fillSharedByKind(value reflect.Value, path string) error {
	if !value.CanSet() {
		return fmt.Errorf("SharedRecord%s cannot be set; export it, or teach fillSharedByKind, recordForShared and parseSharedRecordJSON about it", path)
	}
	switch value.Kind() {
	case reflect.Struct:
		for i := range value.NumField() {
			if err := fillSharedByKind(value.Field(i), path+"."+value.Type().Field(i).Name); err != nil {
				return err
			}
		}
	case reflect.String:
		value.SetString("x")
	case reflect.Int:
		// The only int is the routed port, which must be one of the pinned
		// container ports below.
		value.SetInt(8025)
	case reflect.Map:
		if value.Type() != reflect.TypeOf(map[int]int{}) {
			return fmt.Errorf("SharedRecord%s has map type %s, which this guard does not know how to fill; teach fillSharedByKind, recordForShared and parseSharedRecordJSON about it", path, value.Type())
		}
		value.Set(reflect.ValueOf(map[int]int{1025: 30000, 8025: 30001}))
	default:
		return fmt.Errorf("SharedRecord%s has kind %s (%s), which this guard does not know how to fill; teach fillSharedByKind, recordForShared and parseSharedRecordJSON about it",
			path, value.Kind(), value.Type())
	}
	return nil
}

// TestSharedRecordCarriesEveryField fills every field of SharedRecord, writes
// it with recordForShared and reads it back through the real read path: a
// field added to the struct and left out of the builder or the parser fails
// here, which is how a writer that does not know a field can never erase it.
func TestSharedRecordCarriesEveryField(t *testing.T) {
	filled := SharedRecord{}
	if err := fillSharedByKind(reflect.ValueOf(&filled).Elem(), ""); err != nil {
		t.Fatal(err)
	}
	filled.RepositoryID = "a08b4892-5111-49f1-a47a-5ab988c7f676"
	filled.Name = "mailpit"
	filled.Domain = "acme.localhost"

	store := NewStore(t.TempDir(), filled.RepositoryID)
	file := layout.SharedLayout(store.ControlHome, store.RepositoryID).RecordPath(filled.Name)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := files.AtomicJSON(file, recordForShared(filled)); err != nil {
		t.Fatal(err)
	}
	read, err := store.readSharedRecord(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*read, filled) {
		t.Fatalf("the read path lost a field:\n got  %+v\n want %+v", *read, filled)
	}
}

// TestFillSharedByKindRefusesWhatItCannotFill proves the guard answers a
// field of a kind it does not know with its guiding message and no panic.
func TestFillSharedByKindRefusesWhatItCannotFill(t *testing.T) {
	type future struct{ Routes []string }
	var value future
	err := fillSharedByKind(reflect.ValueOf(&value).Elem().Field(0), ".Routes")
	if err == nil {
		t.Fatal("a slice was filled")
	}
}

// A shared-service record's domain ends up in a routed hostname and on the
// overview page, so a record is read back only with a domain project.domain
// would allow.
func TestParseSharedRecordRefusesADomainNoProjectCouldHave(t *testing.T) {
	record := func(domain string) []byte {
		return []byte(fmt.Sprintf(`{"version": 1, "name": "mailpit", "domain": %q, "ports": {"8025": 30001}}`, domain))
	}
	if parsed, err := parseSharedRecordJSON(record("acme.localhost")); err != nil || parsed.Domain != "acme.localhost" {
		t.Fatalf("a good domain: %+v, %v", parsed, err)
	}
	for _, domain := range []string{"", "Acme.localhost", "acme localhost", "<script>.localhost", "a..b", "-a.localhost", strings.Repeat("a", 64) + ".localhost"} {
		if _, err := parseSharedRecordJSON(record(domain)); err == nil {
			t.Errorf("domain %q was accepted", domain)
		}
	}
}
