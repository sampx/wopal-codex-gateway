package clientfp

import (
	"sync"
)

// Change describes how a request shape differs from the previous one observed
// for the same client version and model. It is the "what changed" record: the
// gateway writes it once per transition instead of leaving the operator to diff
// raw requests after a failure.
type Change struct {
	// Version is the client version that produced the new shape.
	Version string
	// Model is the model the shape was observed for.
	Model string
	// SurfaceFrom and SurfaceTo record a tool-surface transition.
	SurfaceFrom string
	SurfaceTo   string
	// ToolCountFrom and ToolCountTo record the tool-count delta.
	ToolCountFrom int
	ToolCountTo   int
	// FunctionsFrom and FunctionsTo record the functions-namespace contents.
	FunctionsFrom []string
	FunctionsTo   []string
	// KeysAdded and KeysRemoved record top-level parameter changes.
	KeysAdded   []string
	KeysRemoved []string
	// EncodingFrom and EncodingTo record a compression change.
	EncodingFrom string
	EncodingTo   string
	// BetaFeaturesFrom and BetaFeaturesTo record a negotiated-feature change.
	BetaFeaturesFrom string
	BetaFeaturesTo   string
}

// Empty reports whether the change carries no differences.
func (c *Change) Empty() bool {
	if c == nil {
		return true
	}
	return c.SurfaceFrom == c.SurfaceTo &&
		c.ToolCountFrom == c.ToolCountTo &&
		equalStrings(c.FunctionsFrom, c.FunctionsTo) &&
		len(c.KeysAdded) == 0 && len(c.KeysRemoved) == 0 &&
		c.EncodingFrom == c.EncodingTo &&
		c.BetaFeaturesFrom == c.BetaFeaturesTo
}

// Fields returns the change as log fields.
func (c *Change) Fields() map[string]any {
	fields := map[string]any{"event": "client_request_contract_changed"}
	if c.Version != "" {
		fields["client_version"] = c.Version
	}
	if c.Model != "" {
		fields["request_model"] = c.Model
	}
	if c.SurfaceFrom != c.SurfaceTo {
		fields["tool_surface_from"] = c.SurfaceFrom
		fields["tool_surface_to"] = c.SurfaceTo
	}
	if c.ToolCountFrom != c.ToolCountTo {
		fields["tool_count_from"] = c.ToolCountFrom
		fields["tool_count_to"] = c.ToolCountTo
	}
	if !equalStrings(c.FunctionsFrom, c.FunctionsTo) {
		fields["functions_from"] = c.FunctionsFrom
		fields["functions_to"] = c.FunctionsTo
	}
	if len(c.KeysAdded) > 0 {
		fields["keys_added"] = c.KeysAdded
	}
	if len(c.KeysRemoved) > 0 {
		fields["keys_removed"] = c.KeysRemoved
	}
	if c.EncodingFrom != c.EncodingTo {
		fields["encoding_from"] = c.EncodingFrom
		fields["encoding_to"] = c.EncodingTo
	}
	if c.BetaFeaturesFrom != c.BetaFeaturesTo {
		fields["features_from"] = c.BetaFeaturesFrom
		fields["features_to"] = c.BetaFeaturesTo
	}
	return fields
}

// Snapshot is the last shape observed for one key.
type Snapshot struct {
	Shape    Shape
	Encoding string
	Features string
}

// Watcher remembers the last shape per client version and model, so a change
// can be reported once when it happens rather than on every request.
//
// It is bounded: Codex upgrades are frequent and each upgrade may be seen for
// several models, so old versions are evicted once the table is full. Losing an
// entry only costs a repeated "changed" line, never a missed request.
type Watcher struct {
	mu      sync.Mutex
	limit   int
	entries map[string]Snapshot
	order   []string
}

// NewWatcher returns a watcher holding at most limit client/model pairs.
func NewWatcher(limit int) *Watcher {
	if limit <= 0 {
		limit = 64
	}
	return &Watcher{limit: limit, entries: make(map[string]Snapshot, limit)}
}

// Observed records the current shape and returns the change relative to the
// previous one. A nil change means this is the first sighting.
func (w *Watcher) Observed(version, model string, shape Shape, encoding, features string) *Change {
	if w == nil {
		return nil
	}
	key := version + "\x00" + model
	w.mu.Lock()
	defer w.mu.Unlock()

	next := Snapshot{Shape: shape, Encoding: encoding, Features: features}
	previous, seen := w.entries[key]
	if !seen {
		w.store(key, next)
		return nil
	}
	w.entries[key] = next

	change := &Change{
		Version:          version,
		Model:            model,
		SurfaceFrom:      previous.Shape.ToolSurface,
		SurfaceTo:        shape.ToolSurface,
		ToolCountFrom:    previous.Shape.ToolCount,
		ToolCountTo:      shape.ToolCount,
		FunctionsFrom:    previous.Shape.Functions,
		FunctionsTo:      shape.Functions,
		EncodingFrom:     previous.Encoding,
		EncodingTo:       encoding,
		BetaFeaturesFrom: previous.Features,
		BetaFeaturesTo:   features,
	}
	for _, key := range shape.Keys {
		if !contains(previous.Shape.Keys, key) {
			change.KeysAdded = append(change.KeysAdded, key)
		}
	}
	for _, key := range previous.Shape.Keys {
		if !contains(shape.Keys, key) {
			change.KeysRemoved = append(change.KeysRemoved, key)
		}
	}
	if change.Empty() {
		return nil
	}
	return change
}

func (w *Watcher) store(key string, snapshot Snapshot) {
	if len(w.entries) >= w.limit {
		if len(w.order) > 0 {
			delete(w.entries, w.order[0])
			w.order = w.order[1:]
		}
	}
	w.entries[key] = snapshot
	w.order = append(w.order, key)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
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
