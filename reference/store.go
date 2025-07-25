package reference

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/distribution/reference"
	"github.com/moby/sys/atomicwriter"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// ErrDoesNotExist is returned if a reference is not found in the
// store.
var ErrDoesNotExist notFoundError = "reference does not exist"

// An Association is a tuple associating a reference with an image ID.
type Association struct {
	Ref reference.Named
	ID  digest.Digest
}

// Store provides the set of methods which can operate on a reference store.
type Store interface {
	References(id digest.Digest) []reference.Named
	ReferencesByName(ref reference.Named) []Association
	AddTag(ref reference.Named, id digest.Digest, force bool) error
	AddDigest(ref reference.Canonical, id digest.Digest, force bool) error
	Remove(ref reference.Named) (bool, error)
	lookup(ref reference.Named) (digest.Digest, error)
}

type refStore struct {
	mu sync.RWMutex
	jsonPath string
	Repositories map[string]repository
	referencesByIDCache map[digest.Digest]map[string]reference.Named
}

// Repository maps tags to digests. The key is a stringified Reference,
// including the repository name.
type repository map[string]digest.Digest

type lexicalRefs []reference.Named

func (a lexicalRefs) Len() int      { return len(a) }
func (a lexicalRefs) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a lexicalRefs) Less(i, j int) bool {
	return a[i].String() < a[j].String()
}

type lexicalAssociations []Association

func (a lexicalAssociations) Len() int      { return len(a) }
func (a lexicalAssociations) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a lexicalAssociations) Less(i, j int) bool {
	return a[i].Ref.String() < a[j].Ref.String()
}

// NewReferenceStore creates a new reference store, tied to a file path where
// the set of references are serialized in JSON format.
func NewReferenceStore(jsonPath string) (Store, error) {
	abspath, err := filepath.Abs(jsonPath)
	if err != nil {
		return nil, err
	}

	store := &refStore{
		jsonPath:            abspath,
		Repositories:        make(map[string]repository),
		referencesByIDCache: make(map[digest.Digest]map[string]reference.Named),
	}
	// Load the json file if it exists, otherwise create it.
	if err := store.reload(); os.IsNotExist(err) {
		if err := store.save(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return store, nil
}

// AddTag adds a tag reference to the store. If force is set to true, existing
// references can be overwritten. This only works for tags, not digests.
func (store *refStore) AddTag(ref reference.Named, id digest.Digest, force bool) error {
	if _, isCanonical := ref.(reference.Canonical); isCanonical {
		return errors.WithStack(invalidTagError("refusing to create a tag with a digest reference"))
	}
	return store.addReference(reference.TagNameOnly(ref), id, force)
}

// AddDigest adds a digest reference to the store.
func (store *refStore) AddDigest(ref reference.Canonical, id digest.Digest, force bool) error {
	return store.addReference(ref, id, force)
}

func favorDigest(originalRef reference.Named) (reference.Named, error) {
	ref := originalRef
	// If the reference includes a digest and a tag, we must store only the
	// digest.
	canonical, isCanonical := originalRef.(reference.Canonical)
	_, isNamedTagged := originalRef.(reference.NamedTagged)

	if isCanonical && isNamedTagged {
		trimmed, err := reference.WithDigest(reference.TrimNamed(canonical), canonical.Digest())
		if err != nil {
			// should never happen
			return originalRef, err
		}
		ref = trimmed
	}
	return ref, nil
}

func (store *refStore) addReference(ref reference.Named, id digest.Digest, force bool) error {
	ref, err := favorDigest(ref)
	if err != nil {
		return err
	}

	refName := reference.FamiliarName(ref)
	refStr := reference.FamiliarString(ref)

	if refName == string(digest.Canonical) {
		return errors.WithStack(invalidTagError("refusing to create an ambiguous tag using digest algorithm as name"))
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	repo, exists := store.Repositories[refName]
	if !exists || repo == nil {
		repo = make(map[string]digest.Digest)
		store.Repositories[refName] = repo
	}

	oldID, exists := repo[refStr]

	if exists {
		if oldID == id {
			// Nothing to do. The caller may have checked for this using store.Get in advance, but store.mu was unlocked in the meantime, so this can legitimately happen nevertheless.
			return nil
		}

		// force only works for tags
		if digested, isDigest := ref.(reference.Canonical); isDigest {
			return errors.WithStack(conflictingTagError("cannot overwrite digest " + digested.Digest().String()))
		}

		if !force {
			return errors.WithStack(
				conflictingTagError(
					fmt.Sprintf("tag %s is already set to image %s, use the force option to replace it", refStr, oldID.String()),
				),
			)
		}

		if store.referencesByIDCache[oldID] != nil {
			delete(store.referencesByIDCache[oldID], refStr)
			if len(store.referencesByIDCache[oldID]) == 0 {
				delete(store.referencesByIDCache, oldID)
			}
		}
	}

	repo[refStr] = id
	if store.referencesByIDCache[id] == nil {
		store.referencesByIDCache[id] = make(map[string]reference.Named)
	}
	store.referencesByIDCache[id][refStr] = ref

	return store.save()
}

// Delete deletes a reference from the store. It returns true if a deletion
// happened, or false otherwise.
func (store *refStore) Delete(ref reference.Named) (bool, error) {
	ref, err := favorDigest(ref)
	if err != nil {
		return false, err
	}

	ref = reference.TagNameOnly(ref)

	refName := reference.FamiliarName(ref)
	refStr := reference.FamiliarString(ref)

	store.mu.Lock()
	defer store.mu.Unlock()

	repo, exists := store.Repositories[refName]
	if !exists {
		return false, ErrDoesNotExist
	}

	if id, exists := repo[refStr]; exists {
		delete(repo, refStr)
		if len(repo) == 0 {
			delete(store.Repositories, refName)
		}
		if store.referencesByIDCache[id] != nil {
			delete(store.referencesByIDCache[id], refStr)
			if len(store.referencesByIDCache[id]) == 0 {
				delete(store.referencesByIDCache, id)
			}
		}
		return true, store.save()
	}

	return false, ErrDoesNotExist
}

// Get retrieves an item from the store by reference
func (store *refStore) Get(ref reference.Named) (digest.Digest, error) {
	if canonical, ok := ref.(reference.Canonical); ok {
		// If reference contains both tag and digest, only
		// lookup by digest as it takes precedence over
		// tag, until tag/digest combos are stored.
		if _, ok := ref.(reference.Tagged); ok {
			var err error
			ref, err = reference.WithDigest(reference.TrimNamed(canonical), canonical.Digest())
			if err != nil {
				return "", err
			}
		}
	} else {
		ref = reference.TagNameOnly(ref)
	}

	refName := reference.FamiliarName(ref)
	refStr := reference.FamiliarString(ref)

	store.mu.RLock()
	defer store.mu.RUnlock()

	repo, exists := store.Repositories[refName]
	if !exists || repo == nil {
		return "", ErrDoesNotExist
	}

	id, exists := repo[refStr]
	if !exists {
		return "", ErrDoesNotExist
	}

	return id, nil
}

// References returns a slice of references to the given ID. The slice
// will be nil if there are no references to this ID.
func (store *refStore) References(id digest.Digest) []reference.Named {
	store.mu.RLock()
	defer store.mu.RUnlock()

	// Convert the internal map to an array for two reasons:
	// 1) We must not return a mutable
	// 2) It would be ugly to expose the extraneous map keys to callers.

	var references []reference.Named
	for _, ref := range store.referencesByIDCache[id] {
		references = append(references, ref)
	}

	sort.Sort(lexicalRefs(references))

	return references
}

// ReferencesByName returns the references for a given repository name.
// If there are no references known for this repository name,
// ReferencesByName returns nil.
func (store *refStore) ReferencesByName(ref reference.Named) []Association {
	refName := reference.FamiliarName(ref)

	store.mu.RLock()
	defer store.mu.RUnlock()

	repo, exists := store.Repositories[refName]
	if !exists {
		return nil
	}

	var associations []Association
	for refStr, refID := range repo {
		ref, err := reference.ParseNormalizedNamed(refStr)
		if err != nil {
			// Should never happen
			return nil
		}
		associations = append(associations,
			Association{
				Ref: ref,
				ID:  refID,
			})
	}

	sort.Sort(lexicalAssociations(associations))

	return associations
}

func (store *refStore) save() error {
	// Store the json
	jsonData, err := json.Marshal(store)
	if err != nil {
		return err
	}
	return atomicwriter.WriteFile(store.jsonPath, jsonData, 0o600)
}

func (store *refStore) reload() error {
	f, err := os.Open(store.jsonPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&store); err != nil {
		return err
	}

	for _, repo := range store.Repositories {
		for refStr, refID := range repo {
			ref, err := reference.ParseNormalizedNamed(refStr)
			if err != nil {
				// Should never happen
				continue
			}
			if store.referencesByIDCache[refID] == nil {
				store.referencesByIDCache[refID] = make(map[string]reference.Named)
			}
			store.referencesByIDCache[refID][refStr] = ref
		}
	}

	return nil
}
