package main

import "testing"

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
