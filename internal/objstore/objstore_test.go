package objstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDir(t *testing.T) {
	ctx := context.Background()
	d := Dir(t.TempDir())
	if err := d.Put(ctx, "a/dt=1/x.parquet", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, "a/dt=1/x.parquet", []byte("y")); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite: %v", err)
	}
	_ = d.Put(ctx, "a/dt=2/y", []byte("y"))
	_ = d.Put(ctx, "b/z", []byte("z"))
	keys, err := d.List(ctx, "a/")
	if err != nil || !slices.Equal(keys, []string{"a/dt=1/x.parquet", "a/dt=2/y"}) {
		t.Fatalf("list %v %v", keys, err)
	}
	if b, err := d.Get(ctx, "a/dt=1/x.parquet"); err != nil || string(b) != "x" {
		t.Fatalf("get %q %v", b, err)
	}
	st, _ := os.Stat(filepath.Join(string(d), "b", "z"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	for _, k := range []string{"../x", "/etc/passwd", ""} {
		if err := d.Put(ctx, k, nil); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	if keys, err := Dir(filepath.Join(string(d), "missing")).List(ctx, ""); err != nil || len(keys) != 0 {
		t.Errorf("missing dir: %v %v", keys, err)
	}
}
