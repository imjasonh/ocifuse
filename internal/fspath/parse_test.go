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
		{
			in:          "index.docker.io/library/ubuntu:22.04~linux-arm64/etc/os-release",
			wantRef:     "index.docker.io/library/ubuntu:22.04",
			wantInImage: "/etc/os-release",
		},
		{
			in:          "ubuntu:22.04~linux-arm64",
			wantRef:     "index.docker.io/library/ubuntu:22.04",
			wantInImage: "/",
		},
		{
			in:          "gcr.io/distroless/base:nonroot~linux-arm64-v8/usr/bin/sh",
			wantRef:     "gcr.io/distroless/base:nonroot",
			wantInImage: "/usr/bin/sh",
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

func TestParse_Platform(t *testing.T) {
	cases := []struct {
		in           string
		wantOS       string
		wantArch     string
		wantVariant  string
		wantPlatform bool
	}{
		{in: "ubuntu:22.04", wantPlatform: false},
		{in: "ubuntu:22.04~linux-arm64", wantPlatform: true, wantOS: "linux", wantArch: "arm64"},
		{in: "ubuntu:22.04~linux-arm64-v8", wantPlatform: true, wantOS: "linux", wantArch: "arm64", wantVariant: "v8"},
		{in: "ubuntu:22.04~windows-amd64", wantPlatform: true, wantOS: "windows", wantArch: "amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantPlatform {
				if got.Platform != nil {
					t.Errorf("Platform = %+v, want nil", got.Platform)
				}
				return
			}
			if got.Platform == nil {
				t.Fatalf("Platform = nil, want %s/%s", tc.wantOS, tc.wantArch)
			}
			if got.Platform.OS != tc.wantOS {
				t.Errorf("OS = %q, want %q", got.Platform.OS, tc.wantOS)
			}
			if got.Platform.Architecture != tc.wantArch {
				t.Errorf("Architecture = %q, want %q", got.Platform.Architecture, tc.wantArch)
			}
			if got.Platform.Variant != tc.wantVariant {
				t.Errorf("Variant = %q, want %q", got.Platform.Variant, tc.wantVariant)
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
