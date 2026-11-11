package clierr_test

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/sofired/treerig/internal/clierr"
)

func TestErrorIsKind(t *testing.T) {
	t.Run("matches its Kind, and says nothing of a cause", func(t *testing.T) {
		err := clierr.New("gone")
		err.Kind = fs.ErrNotExist
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("errors.Is(err, fs.ErrNotExist) = false")
		}
		if err.Unwrap() != nil {
			t.Fatalf("Unwrap = %v, want no cause", err.Unwrap())
		}
	})

	t.Run("does not match another error", func(t *testing.T) {
		err := clierr.New("gone")
		err.Kind = fs.ErrNotExist
		if errors.Is(err, fs.ErrPermission) {
			t.Fatal("errors.Is(err, fs.ErrPermission) = true")
		}
	})

	t.Run("with no Kind matches nothing, not even a nil target", func(t *testing.T) {
		err := clierr.New("plain")
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, nil) {
			t.Fatal("an error with no Kind matched")
		}
	})

	t.Run("still reaches its Cause", func(t *testing.T) {
		err := clierr.Wrap("outer", fs.ErrPermission)
		err.Kind = fs.ErrNotExist
		if !errors.Is(err, fs.ErrPermission) || !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("want both the cause and the kind")
		}
	})
}
