package store

import (
	"maps"
	"sync"
)

// Platform represents a supported OS/architecture combination.
type Platform struct {
	OS   string
	Arch string
}

// ImageInfo holds the inspection result for an image.
type ImageInfo struct {
	Ref       string // image reference as seen in pod spec
	Digest    string
	Platforms []Platform
}

// Store is a thread-safe registry of image → platforms, with pod reference counting.
type Store struct {
	mu        sync.RWMutex
	images    map[string]*ImageInfo      // imageRef → info (inspection done)
	pending   map[string]struct{}        // imageRef → inspection in progress
	podImages map[string]map[string]bool // podRef → set of imageRefs
}

func New() *Store {
	return &Store{
		images:    make(map[string]*ImageInfo),
		pending:   make(map[string]struct{}),
		podImages: make(map[string]map[string]bool),
	}
}

// TrackPodImage registers that podRef uses imageRef.
// Returns true if the image requires inspection (unknown and not pending).
func (s *Store) TrackPodImage(podRef, imageRef string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.podImages[podRef] == nil {
		s.podImages[podRef] = make(map[string]bool)
	}
	s.podImages[podRef][imageRef] = true

	if _, known := s.images[imageRef]; known {
		return false
	}
	if _, pending := s.pending[imageRef]; pending {
		return false
	}

	s.pending[imageRef] = struct{}{}
	return true
}

// SetImage stores the inspection result.
// Skips storage if no pod references the image anymore (deleted during inspection).
func (s *Store) SetImage(imageRef, digest string, platforms []Platform) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.pending, imageRef)

	for _, imgs := range s.podImages {
		if imgs[imageRef] {
			s.images[imageRef] = &ImageInfo{
				Ref:       imageRef,
				Digest:    digest,
				Platforms: platforms,
			}
			return
		}
	}
	// No pod uses this image anymore; discard result.
}

// FailImage removes imageRef from pending after a failed inspection,
// allowing the next pod event to re-trigger it.
func (s *Store) FailImage(imageRef string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, imageRef)
}

// RemovePod unregisters all images for a pod and removes orphaned entries.
func (s *Store) RemovePod(podRef string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	imageRefs, ok := s.podImages[podRef]
	if !ok {
		return
	}
	delete(s.podImages, podRef)

	for imageRef := range imageRefs {
		if !s.isImageUsed(imageRef) {
			delete(s.images, imageRef)
		}
	}
}

// isImageUsed reports whether any pod still references imageRef.
// Must be called with the lock held.
func (s *Store) isImageUsed(imageRef string) bool {
	for _, imgs := range s.podImages {
		if imgs[imageRef] {
			return true
		}
	}
	return false
}

// Snapshot returns a point-in-time copy of all known images.
// Go 1.23: maps.Values returns iter.Seq[V], iterated with range.
func (s *Store) Snapshot() []ImageInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]ImageInfo, 0, len(s.images))
	for info := range maps.Values(s.images) {
		result = append(result, *info)
	}
	return result
}
