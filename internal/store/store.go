package store

import (
	"maps"
	"sync"

	"github.com/PixiBixi/kubearch/internal/types"
)

// ImageInfo holds the inspection result for an image.
type ImageInfo struct {
	Ref       string // image reference as seen in pod spec
	Digest    string
	Platforms []types.Platform
}

// Store is a thread-safe registry of image → platforms, with pod reference counting.
//
// podImages and imagePods are two views of the same pod↔image relation. The
// reverse index is what keeps every operation independent of cluster size: a
// pod delete or an inspection result touches only the images of that pod,
// never the full pod set.
type Store struct {
	mu        sync.RWMutex
	images    map[string]*ImageInfo          // imageRef → info (inspection done)
	pending   map[string]struct{}            // imageRef → inspection in progress
	podImages map[string]map[string]struct{} // podRef → set of imageRefs
	imagePods map[string]map[string]struct{} // imageRef → set of podRefs
}

func New() *Store {
	return &Store{
		images:    make(map[string]*ImageInfo),
		pending:   make(map[string]struct{}),
		podImages: make(map[string]map[string]struct{}),
		imagePods: make(map[string]map[string]struct{}),
	}
}

// SetPodImages replaces the image set of podRef with imageRefs and returns the
// refs that require inspection (neither known nor already in flight). Images the
// pod no longer references are unlinked, and dropped entirely once no pod uses
// them, which is what makes this safe to call on pod updates, not just adds.
// Those dropped images are returned as orphaned.
func (s *Store) SetPodImages(podRef string, imageRefs []string) (toInspect, orphaned []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.podImages[podRef]
	next := make(map[string]struct{}, len(imageRefs))

	for _, imageRef := range imageRefs {
		if _, dup := next[imageRef]; dup {
			continue
		}
		next[imageRef] = struct{}{}

		if s.imagePods[imageRef] == nil {
			s.imagePods[imageRef] = make(map[string]struct{})
		}
		s.imagePods[imageRef][podRef] = struct{}{}

		if _, known := s.images[imageRef]; known {
			continue
		}
		if _, inFlight := s.pending[imageRef]; inFlight {
			continue
		}
		s.pending[imageRef] = struct{}{}
		toInspect = append(toInspect, imageRef)
	}

	for imageRef := range current {
		if _, still := next[imageRef]; !still && s.unlink(podRef, imageRef) {
			orphaned = append(orphaned, imageRef)
		}
	}
	s.podImages[podRef] = next

	return toInspect, orphaned
}

// SetImage stores the inspection result, replacing any previous one in place so
// a re-inspection never leaves a gap in the metrics.
// Skips storage if no pod references the image anymore (deleted during inspection).
func (s *Store) SetImage(imageRef, digest string, platforms []types.Platform) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.pending, imageRef)

	if len(s.imagePods[imageRef]) == 0 {
		return // no pod uses this image anymore; discard result
	}
	s.images[imageRef] = &ImageInfo{
		Ref:       imageRef,
		Digest:    digest,
		Platforms: platforms,
	}
}

// ClaimPending marks imageRef as in flight and reports whether the caller
// should inspect it: a pod still references it and it has no result yet, or
// allowKnown is set (re-inspection, which keeps the current result visible).
// On a refusal pending is cleared, since the queue is the only owner of that slot.
func (s *Store) ClaimPending(imageRef string, allowKnown bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, known := s.images[imageRef]
	if (known && !allowKnown) || len(s.imagePods[imageRef]) == 0 {
		delete(s.pending, imageRef)
		return false
	}
	s.pending[imageRef] = struct{}{}
	return true
}

// State reports whether any pod references imageRef and whether it has an
// inspection result.
func (s *Store) State(imageRef string) (referenced, known bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, known = s.images[imageRef]
	return len(s.imagePods[imageRef]) > 0, known
}

// FailImage removes imageRef from pending after a failed inspection,
// allowing the next pod event to re-trigger it.
func (s *Store) FailImage(imageRef string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, imageRef)
}

// RemovePod unregisters all images for a pod, removes orphaned entries and
// returns their refs.
func (s *Store) RemovePod(podRef string) (orphaned []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	imageRefs, ok := s.podImages[podRef]
	if !ok {
		return nil
	}
	delete(s.podImages, podRef)

	for imageRef := range imageRefs {
		if s.unlink(podRef, imageRef) {
			orphaned = append(orphaned, imageRef)
		}
	}
	return orphaned
}

// unlink drops the podRef→imageRef edge and forgets the image once it is
// orphaned, reporting whether it was. Must be called with the write lock held.
func (s *Store) unlink(podRef, imageRef string) bool {
	pods := s.imagePods[imageRef]
	delete(pods, podRef)
	if len(pods) > 0 {
		return false
	}
	delete(s.imagePods, imageRef)
	delete(s.images, imageRef)
	// pending is deliberately left alone: an in-flight inspection still owns
	// that slot and clears it via SetImage/FailImage/ClaimPending.
	return true
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

// Stats reports the current store size, for self-monitoring metrics.
func (s *Store) Stats() (images, pending, pods int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.images), len(s.pending), len(s.podImages)
}
