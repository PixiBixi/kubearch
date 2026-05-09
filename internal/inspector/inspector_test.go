package inspector

import (
	"context"
	"log/slog"
	"os"
	"testing"

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
	// Confirm zero value of PodAuth is usable (no panics on construction).
	auth := PodAuth{}
	if auth.Namespace != "" || auth.ServiceAccountName != "" || auth.ImagePullSecrets != nil {
		t.Error("zero PodAuth should have empty fields")
	}
}
