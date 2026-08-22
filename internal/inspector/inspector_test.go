package inspector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	goregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
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
			Add:      img1,
			Platform: &v1.Platform{OS: "linux", Architecture: "amd64"},
		},
		mutate.IndexAddendum{
			Add:      img2,
			Platform: &v1.Platform{OS: "linux", Architecture: "arm64"},
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

// --- credential caching ---

// pushSingleArch publishes a throwaway linux/amd64 image and returns its ref.
func pushSingleArch(t *testing.T, host, repo string) string {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	imageRef := host + "/" + repo + ":latest"
	ref, err := name.ParseReference(imageRef, name.Insecure)
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return imageRef
}

// apiGets counts read calls the inspector made against the API server.
func apiGets(client *fake.Clientset, resource string) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

func newTestInspector(t *testing.T, client *fake.Clientset) *Inspector {
	t.Helper()
	insp := New(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return insp
}

// Building a keychain costs a live ServiceAccount GET plus one GET per pull
// secret. Every image of a workload resolves to the same credentials, so the
// API server must be hit once, not once per image.
func TestInspect_ReusesCredentialsAcrossImages(t *testing.T) {
	host := startTestRegistry(t)
	client := fake.NewClientset()
	insp := newTestInspector(t, client)

	auth := PodAuth{Namespace: "prod", ServiceAccountName: "app"}
	for _, repo := range []string{"test/a", "test/b", "test/c"} {
		imageRef := pushSingleArch(t, host, repo)
		if _, _, err := insp.Inspect(context.Background(), imageRef, auth); err != nil {
			t.Fatalf("Inspect %s: %v", imageRef, err)
		}
	}

	if got := apiGets(client, "serviceaccounts"); got != 1 {
		t.Errorf("ServiceAccount GETs = %d, want 1 for 3 images sharing credentials", got)
	}
}

func TestInspect_SeparateCredentialsPerNamespace(t *testing.T) {
	host := startTestRegistry(t)
	client := fake.NewClientset()
	insp := newTestInspector(t, client)

	imageRef := pushSingleArch(t, host, "test/shared")
	for _, ns := range []string{"prod", "staging"} {
		auth := PodAuth{Namespace: ns, ServiceAccountName: "app"}
		if _, _, err := insp.Inspect(context.Background(), imageRef, auth); err != nil {
			t.Fatalf("Inspect in %s: %v", ns, err)
		}
	}

	if got := apiGets(client, "serviceaccounts"); got != 2 {
		t.Errorf("ServiceAccount GETs = %d, want 2 for two namespaces", got)
	}
}

func TestInspect_RebuildsCredentialsAfterTTL(t *testing.T) {
	host := startTestRegistry(t)
	client := fake.NewClientset()
	insp := newTestInspector(t, client)

	clock := time.Now()
	insp.now = func() time.Time { return clock }

	imageRef := pushSingleArch(t, host, "test/ttl")
	auth := PodAuth{Namespace: "prod", ServiceAccountName: "app"}

	if _, _, err := insp.Inspect(context.Background(), imageRef, auth); err != nil {
		t.Fatalf("first Inspect: %v", err)
	}
	clock = clock.Add(credTTL + time.Second)
	if _, _, err := insp.Inspect(context.Background(), imageRef, auth); err != nil {
		t.Fatalf("second Inspect: %v", err)
	}

	if got := apiGets(client, "serviceaccounts"); got != 2 {
		t.Errorf("ServiceAccount GETs = %d, want 2 (credentials must not be cached past their TTL)", got)
	}
}

// Concurrent misses on the same credentials must collapse into one API call,
// which is exactly the startup pattern: N workers, one namespace.
func TestInspect_ConcurrentMissesShareOneBuild(t *testing.T) {
	host := startTestRegistry(t)
	client := fake.NewClientset()
	insp := newTestInspector(t, client)

	imageRef := pushSingleArch(t, host, "test/concurrent")
	auth := PodAuth{Namespace: "prod", ServiceAccountName: "app"}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			if _, _, err := insp.Inspect(context.Background(), imageRef, auth); err != nil {
				t.Errorf("Inspect: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := apiGets(client, "serviceaccounts"); got != 1 {
		t.Errorf("ServiceAccount GETs = %d, want 1 for 8 concurrent inspections", got)
	}
}

func TestPodAuth_CacheKey(t *testing.T) {
	a := PodAuth{Namespace: "ns", ServiceAccountName: "sa", ImagePullSecrets: []string{"b", "a"}}
	b := PodAuth{Namespace: "ns", ServiceAccountName: "sa", ImagePullSecrets: []string{"a", "b"}}
	if a.cacheKey() != b.cacheKey() {
		t.Error("pull secret order must not produce a different cache key")
	}

	other := PodAuth{Namespace: "ns", ServiceAccountName: "sa", ImagePullSecrets: []string{"a"}}
	if a.cacheKey() == other.cacheKey() {
		t.Error("a different pull secret set must produce a different cache key")
	}

	// Guard against a separator collision between the fields.
	x := PodAuth{Namespace: "a", ServiceAccountName: "b"}
	y := PodAuth{Namespace: "a\x00b"}
	if x.cacheKey() == y.cacheKey() {
		t.Error("field boundaries must not collide")
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unauthorized", &transport.Error{StatusCode: http.StatusUnauthorized}, true},
		{"forbidden", &transport.Error{StatusCode: http.StatusForbidden}, true},
		{"not found", &transport.Error{StatusCode: http.StatusNotFound}, false},
		{"wrapped unauthorized", fmt.Errorf("fetch: %w", &transport.Error{StatusCode: http.StatusUnauthorized}), true},
		{"plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthError(tc.err); got != tc.want {
				t.Errorf("isAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
