package mount

import (
	"archive/tar"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
)

func TestClassifySegment(t *testing.T) {
	cases := []struct {
		name        string
		accumulated string
		child       string
		wantKind    segmentKind
		wantRefName string // empty if no ref expected
		wantErr     bool
	}{
		{
			name:        "first registry segment is intermediate",
			accumulated: "",
			child:       "index.docker.io",
			wantKind:    segIntermediate,
		},
		{
			name:        "repo segment under registry is intermediate",
			accumulated: "index.docker.io",
			child:       "library",
			wantKind:    segIntermediate,
		},
		{
			name:        "tag-bearing segment is tag ref",
			accumulated: "index.docker.io/library",
			child:       "ubuntu:latest",
			wantKind:    segTagRef,
			wantRefName: "index.docker.io/library/ubuntu:latest",
		},
		{
			name:        "digest-bearing segment is digest ref",
			accumulated: "index.docker.io/library",
			child:       "ubuntu@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			wantKind:    segDigestRef,
			wantRefName: "index.docker.io/library/ubuntu@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name:        "shorthand single-segment tag",
			accumulated: "",
			child:       "ubuntu:latest",
			wantKind:    segTagRef,
			wantRefName: "index.docker.io/library/ubuntu:latest",
		},
		{
			name:        "registry with port doesn't trip ref boundary",
			accumulated: "",
			child:       "localhost:5000",
			wantKind:    segIntermediate,
		},
		{
			name:    "malformed ref",
			child:   "ubuntu:::",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, ref, _, _, err := classifySegment(tc.accumulated, tc.child)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %d, want %d", kind, tc.wantKind)
			}
			if tc.wantRefName == "" {
				if ref != nil {
					t.Errorf("ref = %q, want nil", ref.Name())
				}
				return
			}
			if ref == nil {
				t.Fatalf("ref = nil, want %q", tc.wantRefName)
			}
			if ref.Name() != tc.wantRefName {
				t.Errorf("ref.Name() = %q, want %q", ref.Name(), tc.wantRefName)
			}
		})
	}
}

func TestTagSymlinkTarget(t *testing.T) {
	cases := []struct {
		ref    string
		digest string
		want   string
	}{
		{
			ref:    "index.docker.io/library/ubuntu:latest",
			digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			want:   "ubuntu@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
		{
			// Multi-segment repo — basename of the repo, not the registry.
			ref:    "gcr.io/distroless/base:nonroot",
			digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			want:   "base@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			ref, err := name.ParseReference(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			d, err := v1.NewHash(tc.digest)
			if err != nil {
				t.Fatal(err)
			}
			got := tagSymlinkTarget(ref, d)
			if got != tc.want {
				t.Errorf("tagSymlinkTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetAttrFromHeader(t *testing.T) {
	cases := []struct {
		name     string
		header   tar.Header
		isDir    bool
		wantMode uint32
		wantSize uint64
	}{
		{
			name:     "regular file with explicit mode",
			header:   tar.Header{Typeflag: tar.TypeReg, Mode: 0o644, Size: 42},
			wantMode: gofuse.S_IFREG | 0o644,
			wantSize: 42,
		},
		{
			name:     "directory with explicit mode",
			header:   tar.Header{Typeflag: tar.TypeDir, Mode: 0o755},
			isDir:    true,
			wantMode: gofuse.S_IFDIR | 0o755,
		},
		{
			name:     "symlink",
			header:   tar.Header{Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "/bin/busybox"},
			wantMode: gofuse.S_IFLNK | 0o777,
		},
		{
			name:     "file with zero mode defaults to 0644",
			header:   tar.Header{Typeflag: tar.TypeReg, Mode: 0, Size: 0},
			wantMode: gofuse.S_IFREG | 0o644,
		},
		{
			name:     "directory with zero mode defaults to 0755",
			header:   tar.Header{Typeflag: tar.TypeDir, Mode: 0},
			isDir:    true,
			wantMode: gofuse.S_IFDIR | 0o755,
		},
		{
			name:     "high bits in mode are masked off",
			header:   tar.Header{Typeflag: tar.TypeReg, Mode: 0o100644}, // c_isreg | 0o644
			wantMode: gofuse.S_IFREG | 0o644,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attr gofuse.Attr
			setAttrFromHeader(&attr, &tc.header, tc.isDir)
			if attr.Mode != tc.wantMode {
				t.Errorf("Mode = %#o, want %#o", attr.Mode, tc.wantMode)
			}
			if attr.Size != tc.wantSize {
				t.Errorf("Size = %d, want %d", attr.Size, tc.wantSize)
			}
		})
	}
}

func TestJoinInImage(t *testing.T) {
	cases := []struct {
		parent string
		child  string
		want   string
	}{
		{"/", "etc", "/etc"},
		{"/", "usr", "/usr"},
		{"/etc", "os-release", "/etc/os-release"},
		{"/etc/foo", "bar", "/etc/foo/bar"},
	}
	for _, tc := range cases {
		got := joinInImage(tc.parent, tc.child)
		if got != tc.want {
			t.Errorf("joinInImage(%q,%q) = %q, want %q", tc.parent, tc.child, got, tc.want)
		}
	}
}
