package fspath

import (
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in           string
		wantRef      string // empty if Ref should be nil
		wantInImage  string
		wantInterSeg []string
		wantErr      bool
	}{
		{
			in: "",
		},
		{
			in: "/",
		},
		{
			in:           "gcr.io",
			wantInterSeg: []string{"gcr.io"},
		},
		{
			in:           "gcr.io/distroless",
			wantInterSeg: []string{"gcr.io", "distroless"},
		},
		{
			in:           "index.docker.io/library/ubuntu",
			wantInterSeg: []string{"index.docker.io", "library", "ubuntu"},
		},
		{
			in:          "ubuntu:latest",
			wantRef:     "index.docker.io/library/ubuntu:latest",
			wantInImage: "/",
		},
		{
			in:          "ubuntu:latest/etc/os-release",
			wantRef:     "index.docker.io/library/ubuntu:latest",
			wantInImage: "/etc/os-release",
		},
		{
			in:          "library/ubuntu:latest/etc",
			wantRef:     "index.docker.io/library/ubuntu:latest",
			wantInImage: "/etc",
		},
		{
			in:          "index.docker.io/library/ubuntu:latest/etc/os-release",
			wantRef:     "index.docker.io/library/ubuntu:latest",
			wantInImage: "/etc/os-release",
		},
		{
			in:          "gcr.io/distroless/base:nonroot/usr/bin/sh",
			wantRef:     "gcr.io/distroless/base:nonroot",
			wantInImage: "/usr/bin/sh",
		},
		{
			in:          "gcr.io/distroless/base@sha256:0000000000000000000000000000000000000000000000000000000000000000/usr/bin/sh",
			wantRef:     "gcr.io/distroless/base@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			wantInImage: "/usr/bin/sh",
		},
		{
			in:          "localhost:5000/foo/bar:tag/etc",
			wantRef:     "localhost:5000/foo/bar:tag",
			wantInImage: "/etc",
		},
		{
			// Bare "localhost" is not a registry per go-containerregistry; it
			// becomes a Docker Hub user. Use localhost:<port> for a real local
			// registry.
			in:          "localhost/foo:tag",
			wantRef:     "index.docker.io/localhost/foo:tag",
			wantInImage: "/",
		},
		{
			// ':' in an in-image filename must not be reinterpreted as a ref boundary.
			in:          "ubuntu:latest/var/cache/foo:bar",
			wantRef:     "index.docker.io/library/ubuntu:latest",
			wantInImage: "/var/cache/foo:bar",
		},
		{
			in:          "/ubuntu:latest/etc/os-release/",
			wantRef:     "index.docker.io/library/ubuntu:latest",
			wantInImage: "/etc/os-release",
		},
		{
			in:      "ubuntu:::",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantRef == "" {
				if got.Ref != nil {
					t.Errorf("Ref = %q, want nil", got.Ref.Name())
				}
			} else {
				if got.Ref == nil {
					t.Fatalf("Ref = nil, want %q", tc.wantRef)
				}
				if got.Ref.Name() != tc.wantRef {
					t.Errorf("Ref = %q, want %q", got.Ref.Name(), tc.wantRef)
				}
			}

			if got.InImage != tc.wantInImage {
				t.Errorf("InImage = %q, want %q", got.InImage, tc.wantInImage)
			}

			if !equalSegs(got.Intermediate, tc.wantInterSeg) {
				t.Errorf("Intermediate = %v, want %v", got.Intermediate, tc.wantInterSeg)
			}
		})
	}
}

func equalSegs(a, b []string) bool {
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
