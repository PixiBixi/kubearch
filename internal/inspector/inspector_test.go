package inspector

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	goregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNew(t *testing.T) {
	client := fake.NewClientset()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	insp := New(client, logger)
	if insp == nil {
		t.Fatal("New() returned nil")
	}
}

func TestInspect_InvalidReference(t *testing.T) {
	client := fake.NewClientset()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	insp := New(client, logger)

	_, _, err := insp.Inspect(context.Background(), ":::invalid:::", PodAuth{})
	if err == nil {
		t.Error("Inspect with invalid image reference should return an error")
	}
}

func TestPodAuth_Zero(t *testing.T) {
	// Confirm zero value of PodAuth is constructible without panicking.
	_ = PodAuth{}
}

// startTestRegistry starts a local in-memory OCI registry and returns its host address.
func startTestRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(goregistry.New())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestInspect_MultiArch(t *testing.T) {
	host := startTestRegistry(t)

	// Build two random single-arch images to be combined into an index.
	img1, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	img2, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}

	// Assemble a multi-arch OCI image index with explicit platform descriptors.
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add: img1,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "linux", Architecture: "amd64"},
			},
		},
		mutate.IndexAddendum{
			Add: img2,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "linux", Architecture: "arm64"},
			},
		},
	)

	imageRef := host + "/test/multi:latest"
	ref, err := name.ParseReference(imageRef, name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	insp := New(fake.NewClientset(), logger)

	digest, platforms, err := insp.Inspect(context.Background(), imageRef, PodAuth{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if digest == "" {
		t.Error("expected non-empty digest")
	}
	if len(platforms) != 2 {
		t.Fatalf("expected 2 platforms, got %d: %v", len(platforms), platforms)
	}

	// Verify both expected platforms are present (order is not guaranteed).
	found := make(map[string]bool)
	for _, p := range platforms {
		found[p.OS+"/"+p.Arch] = true
	}
	for _, want := range []string{"linux/amd64", "linux/arm64"} {
		if !found[want] {
			t.Errorf("platform %q not found in %v", want, platforms)
		}
	}
}

func TestInspect_SingleArch(t *testing.T) {
	host := startTestRegistry(t)

	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	// Set explicit OS/arch in the image config so the inspector can read them.
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		OS:           "linux",
		Architecture: "amd64",
	})
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}

	imageRef := host + "/test/single:latest"
	ref, err := name.ParseReference(imageRef, name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("Write: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	insp := New(fake.NewClientset(), logger)

	digest, platforms, err := insp.Inspect(context.Background(), imageRef, PodAuth{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if digest == "" {
		t.Error("expected non-empty digest")
	}
	if len(platforms) != 1 {
		t.Fatalf("expected 1 platform, got %d: %v", len(platforms), platforms)
	}
	if platforms[0].OS != "linux" || platforms[0].Arch != "amd64" {
		t.Errorf("unexpected platform: %+v", platforms[0])
	}
}
