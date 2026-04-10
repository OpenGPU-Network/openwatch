package docker

import (
	"testing"
)

func TestReconcileEnv(t *testing.T) {
	cases := []struct {
		name      string
		container []string
		oldImage  []string
		newImage  []string
		want      []string
	}{
		{
			name:      "image-only env bumped across update",
			container: []string{"PATH=/usr/bin", "TEST_VERSION=v1"},
			oldImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v1"},
			newImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v2"},
			want:      []string{"PATH=/usr/bin", "TEST_VERSION=v2"},
		},
		{
			name:      "user override survives update and replaces in place",
			container: []string{"PATH=/usr/bin", "TEST_VERSION=custom"},
			oldImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v1"},
			newImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v2"},
			want:      []string{"PATH=/usr/bin", "TEST_VERSION=custom"},
		},
		{
			name:      "user-added key absent from both images appended",
			container: []string{"PATH=/usr/bin", "TEST_VERSION=v1", "EXTRA=yes"},
			oldImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v1"},
			newImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v2"},
			want:      []string{"PATH=/usr/bin", "TEST_VERSION=v2", "EXTRA=yes"},
		},
		{
			name:      "new image adds a key the user has not touched",
			container: []string{"PATH=/usr/bin"},
			oldImage:  []string{"PATH=/usr/bin"},
			newImage:  []string{"PATH=/usr/bin", "LANG=C"},
			want:      []string{"PATH=/usr/bin", "LANG=C"},
		},
		{
			name:      "new image drops an image-only key",
			container: []string{"PATH=/usr/bin", "LEGACY=1"},
			oldImage:  []string{"PATH=/usr/bin", "LEGACY=1"},
			newImage:  []string{"PATH=/usr/bin"},
			want:      []string{"PATH=/usr/bin"},
		},
		{
			name:      "new image drops a key the user had overridden — override is preserved as an appended entry",
			container: []string{"PATH=/usr/bin", "LEGACY=mine"},
			oldImage:  []string{"PATH=/usr/bin", "LEGACY=1"},
			newImage:  []string{"PATH=/usr/bin"},
			want:      []string{"PATH=/usr/bin", "LEGACY=mine"},
		},
		{
			name:      "malformed bare entry in image is copied across",
			container: []string{"PATH=/usr/bin"},
			oldImage:  []string{"PATH=/usr/bin", "MALFORMED"},
			newImage:  []string{"PATH=/usr/bin", "MALFORMED"},
			want:      []string{"PATH=/usr/bin", "MALFORMED"},
		},
		{
			name:      "empty container env still gets new image defaults",
			container: nil,
			oldImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v1"},
			newImage:  []string{"PATH=/usr/bin", "TEST_VERSION=v2"},
			want:      []string{"PATH=/usr/bin", "TEST_VERSION=v2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileEnv(tc.container, tc.oldImage, tc.newImage)
			if !stringSliceEqual(got, tc.want) {
				t.Fatalf("reconcileEnv mismatch\n  got:  %v\n  want: %v", got, tc.want)
			}
		})
	}
}

func TestReconcileSlice(t *testing.T) {
	t.Run("container matches old image: take new image value", func(t *testing.T) {
		got := reconcileSlice([]string{"sh", "-c", "old"}, []string{"sh", "-c", "old"}, []string{"sh", "-c", "new"})
		want := []string{"sh", "-c", "new"}
		if !stringSliceEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("container diverges: keep container value", func(t *testing.T) {
		got := reconcileSlice([]string{"echo", "hi"}, []string{"sh", "-c", "old"}, []string{"sh", "-c", "new"})
		want := []string{"echo", "hi"}
		if !stringSliceEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("both nil: return new image (itself nil)", func(t *testing.T) {
		got := reconcileSlice[[]string](nil, nil, nil)
		if got != nil {
			t.Fatalf("got %v want nil", got)
		}
	})
	t.Run("length differs: treat as user override", func(t *testing.T) {
		got := reconcileSlice([]string{"sh", "-c", "old", "extra"}, []string{"sh", "-c", "old"}, []string{"sh", "-c", "new"})
		want := []string{"sh", "-c", "old", "extra"}
		if !stringSliceEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
}

func TestReconcileScalar(t *testing.T) {
	if got := reconcileScalar("/app", "/app", "/srv"); got != "/srv" {
		t.Fatalf("inherited scalar not refreshed: got %q", got)
	}
	if got := reconcileScalar("/custom", "/app", "/srv"); got != "/custom" {
		t.Fatalf("user override not preserved: got %q", got)
	}
	if got := reconcileScalar("", "", "/srv"); got != "/srv" {
		t.Fatalf("empty defaults should still refresh: got %q", got)
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
