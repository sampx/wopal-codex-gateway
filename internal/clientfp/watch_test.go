package clientfp

import "testing"

func TestWatcherReportsFirstSightingOnce(t *testing.T) {
	watcher := NewWatcher(8)
	shape := shapeOf(t, codeModeBody)

	if change := watcher.Observed("0.164.0", "gpt-6.1-sol", shape, "zstd", "remote_compaction_v2"); change != nil {
		t.Fatalf("first sighting reported a change: %#v", change)
	}
	// An unchanged repeat must stay quiet, otherwise a healthy session would
	// emit one "changed" line per turn.
	if change := watcher.Observed("0.164.0", "gpt-6.1-sol", shape, "zstd", "remote_compaction_v2"); change != nil {
		t.Fatalf("repeat sighting reported a change: %#v", change)
	}
}

func TestWatcherReportsToolSurfaceLoss(t *testing.T) {
	watcher := NewWatcher(8)
	code := shapeOf(t, codeModeBody)
	direct := shapeOf(t, directBody)

	watcher.Observed("0.164.0", "gpt-6.1-sol", code, "zstd", "")
	change := watcher.Observed("0.164.0", "wopal-ai/deepseek-v4.1-flash", direct, "zstd", "")
	if change == nil {
		// A different model is a different key, so the first sighting is silent.
		// Record it and re-run the transition on the same key.
		watcher.Observed("0.164.0", "wopal-ai/deepseek-v4.1-flash", code, "zstd", "")
		change = watcher.Observed("0.164.0", "wopal-ai/deepseek-v4.1-flash", direct, "zstd", "")
	}
	if change == nil {
		t.Fatal("tool surface change was not reported")
	}
	if change.SurfaceFrom != "code_mode" || change.SurfaceTo != "direct" {
		t.Fatalf("surface change = %s -> %s, want code_mode -> direct", change.SurfaceFrom, change.SurfaceTo)
	}
	if len(change.FunctionsTo) != 0 {
		t.Fatalf("functions_to = %v, want empty", change.FunctionsTo)
	}
	fields := change.Fields()
	if fields["event"] != "client_request_contract_changed" {
		t.Fatalf("event = %v", fields["event"])
	}
	if fields["tool_surface_from"] != "code_mode" || fields["tool_surface_to"] != "direct" {
		t.Fatalf("fields = %#v", fields)
	}
}

func TestWatcherReportsEncodingAndFeatureDrift(t *testing.T) {
	watcher := NewWatcher(8)
	shape := shapeOf(t, codeModeBody)

	watcher.Observed("0.164.0", "gpt-6.1-sol", shape, "zstd", "remote_compaction_v2")
	change := watcher.Observed("0.165.0", "gpt-6.1-sol", shape, "gzip", "remote_compaction_v3")
	if change != nil {
		t.Fatalf("a new version is a new key and must not diff against the old one: %#v", change)
	}

	change = watcher.Observed("0.165.0", "gpt-6.1-sol", shape, "br", "remote_compaction_v3")
	if change == nil {
		t.Fatal("encoding change was not reported")
	}
	if change.EncodingFrom != "gzip" || change.EncodingTo != "br" {
		t.Fatalf("encoding change = %s -> %s", change.EncodingFrom, change.EncodingTo)
	}
}

func TestWatcherReportsKeyDrift(t *testing.T) {
	watcher := NewWatcher(8)
	direct := shapeOf(t, directBody)
	watcher.Observed("0.164.0", "gpt-6.1-sol", direct, "", "")

	// A release that starts sending a new parameter.
	withNewKey := direct
	withNewKey.Keys = append(append([]string(nil), direct.Keys...), "previous_response_id")
	change := watcher.Observed("0.164.0", "gpt-6.1-sol", withNewKey, "", "")
	if change == nil {
		t.Fatal("key addition was not reported")
	}
	if len(change.KeysAdded) != 1 || change.KeysAdded[0] != "previous_response_id" {
		t.Fatalf("keys added = %v", change.KeysAdded)
	}
}

func TestWatcherEvictsOldVersionsWithoutLosingRequests(t *testing.T) {
	watcher := NewWatcher(2)
	shape := shapeOf(t, directBody)
	for _, version := range []string{"1", "2", "3"} {
		watcher.Observed(version, "m", shape, "", "")
	}
	// The oldest key was evicted, so it looks new again. That is a duplicate log
	// line, never a failure.
	if change := watcher.Observed("1", "m", shape, "", ""); change != nil {
		t.Fatalf("evicted key reported a change: %#v", change)
	}
}

func TestNilWatcherIsSafe(t *testing.T) {
	var watcher *Watcher
	if change := watcher.Observed("v", "m", Shape{}, "", ""); change != nil {
		t.Fatal("nil watcher reported a change")
	}
}
