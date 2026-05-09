package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

func TestNsLabel_Empty(t *testing.T) {
	if got := nsLabel(""); got != "all" {
		t.Errorf("nsLabel(%q) = %q, want %q", "", got, "all")
	}
}

func TestNsLabel_NonEmpty(t *testing.T) {
	if got := nsLabel("production"); got != "production" {
		t.Errorf("nsLabel(%q) = %q, want %q", "production", got, "production")
	}
}

func TestBuildK8sClient_InvalidPath(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := buildK8sClient("/nonexistent/kubeconfig.yaml", "", logger)
	if err == nil {
		t.Error("expected error for non-existent kubeconfig path")
	}
}

// minimalKubeconfig writes a syntactically valid kubeconfig with a fake server
// to a temp file and returns the path. The file is registered for cleanup.
func minimalKubeconfig(t *testing.T, contextName string) string {
	t.Helper()
	content := "apiVersion: v1\nkind: Config\nclusters:\n- name: fake\n  cluster:\n    server: https://localhost:0\ncurrent-context: " + contextName + "\ncontexts:\n- name: " + contextName + "\n  context:\n    cluster: fake\n    user: fake\nusers:\n- name: fake\n  user: {}\n"
	f, err := os.CreateTemp("", "kubeconfig-*.yaml")
	if err != nil {
		t.Fatalf("create temp kubeconfig: %v", err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp kubeconfig: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestBuildK8sClient_ValidKubeconfig(t *testing.T) {
	path := minimalKubeconfig(t, "test-ctx")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := buildK8sClient(path, "", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Error("expected non-nil client")
	}
}

func TestBuildK8sClient_WithContext(t *testing.T) {
	path := minimalKubeconfig(t, "test-ctx")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := buildK8sClient(path, "test-ctx", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Error("expected non-nil client")
	}
}

func TestBuildK8sClient_InvalidContext(t *testing.T) {
	path := minimalKubeconfig(t, "test-ctx")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := buildK8sClient(path, "nonexistent-context", logger)
	if err == nil {
		t.Error("expected error for context not found in kubeconfig")
	}
}
