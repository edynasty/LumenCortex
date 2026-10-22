package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/pluginapi"
)

type pluginProbe struct {
	manifest pluginapi.Manifest
	start    func(context.Context, pluginapi.Host) error
	stop     func(context.Context) error
}

func (p *pluginProbe) Manifest() pluginapi.Manifest { return p.manifest }
func (p *pluginProbe) Start(ctx context.Context, host pluginapi.Host) error {
	if p.start != nil {
		return p.start(ctx, host)
	}
	return nil
}
func (p *pluginProbe) Stop(ctx context.Context) error {
	if p.stop != nil {
		return p.stop(ctx)
	}
	return nil
}

func probeManifest(id string, capabilities ...string) pluginapi.Manifest {
	return pluginapi.Manifest{ID: id, Name: id, Version: "1.0.0", APIVersion: pluginapi.APIVersion, Capabilities: capabilities}
}

func newTestRegistry(t *testing.T) (*PluginRegistry, *eventBus, string) {
	t.Helper()
	events := newEventBus()
	t.Cleanup(events.close)
	statePath := filepath.Join(t.TempDir(), PluginStateFileName)
	return newPluginRegistry(events, newHookDispatcher(events), statePath), events, statePath
}

func pluginState(t *testing.T, r *PluginRegistry, id string) PluginInfo {
	t.Helper()
	for _, info := range r.Catalog().Plugins {
		if info.Manifest.ID == id {
			return info
		}
	}
	t.Fatalf("plugin %s not in catalog", id)
	return PluginInfo{}
}

func TestPluginRegistryStartsInIDOrderAndStopsInReverse(t *testing.T) {
	registry, _, _ := newTestRegistry(t)
	var lifecycle []string
	for _, id := range []string{"zeta", "alpha"} {
		if err := registry.Register(&pluginProbe{manifest: probeManifest(id), start: func(context.Context, pluginapi.Host) error {
			lifecycle = append(lifecycle, "start:"+id)
			return nil
		}, stop: func(context.Context) error {
			lifecycle = append(lifecycle, "stop:"+id)
			return nil
		}}, false); err != nil {
			t.Fatal(err)
		}
	}
	registry.StartAll(context.Background())
	if err := registry.Register(&pluginProbe{manifest: probeManifest("late")}, false); !errors.Is(err, ErrPluginRegistryStarted) {
		t.Fatalf("register after start error = %v", err)
	}
	if err := registry.StopAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"start:alpha", "start:zeta", "stop:zeta", "stop:alpha"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("lifecycle = %v, want %v", lifecycle, want)
	}
}

func TestPluginRegistryRejectsInvalidManifests(t *testing.T) {
	registry, _, _ := newTestRegistry(t)
	badVersion := probeManifest("bad-version")
	badVersion.APIVersion++
	cases := map[string]struct {
		plugin pluginapi.Plugin
		want   error
	}{
		"api version":               {&pluginProbe{manifest: badVersion}, ErrPluginAPIVersion},
		"unknown capability":        {&pluginProbe{manifest: probeManifest("future", "telemetry.export")}, ErrPluginCapability},
		"duplicate capability":      {&pluginProbe{manifest: probeManifest("dup", pluginapi.CapabilityEvents, pluginapi.CapabilityEvents)}, nil},
		"bad id":                    {&pluginProbe{manifest: probeManifest("Bad ID")}, nil},
		"undeclared implementation": {&pluginProbe{manifest: probeManifest("fake-source", pluginapi.CapabilitySessionImport)}, nil},
	}
	for name, tc := range cases {
		err := registry.Register(tc.plugin, false)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: Register error = %v, want %v", name, err, tc.want)
		}
	}
	if err := registry.Register(&pluginProbe{manifest: probeManifest("valid")}, false); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&pluginProbe{manifest: probeManifest("valid")}, false); err == nil {
		t.Fatal("duplicate plugin id was accepted")
	}
	if err := registry.Register(&fakeSource{id: "one", source: "omp"}, false); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&fakeSource{id: "two", source: "omp"}, false); err == nil {
		t.Fatal("a second provider of the same import source was accepted")
	}
}

// A plugin that fails or panics on start is isolated: it is marked failed with its error and the
// other plugins still run.
func TestPluginStartFailureIsIsolated(t *testing.T) {
	registry, events, _ := newTestRegistry(t)
	stream, cancel := events.subscribe(8)
	defer cancel()
	for _, p := range []*pluginProbe{
		{manifest: probeManifest("a-broken"), start: func(context.Context, pluginapi.Host) error { return errors.New("startup failed") }},
		{manifest: probeManifest("b-panics"), start: func(context.Context, pluginapi.Host) error { panic("boom") }},
		{manifest: probeManifest("c-healthy")},
	} {
		if err := registry.Register(p, false); err != nil {
			t.Fatal(err)
		}
	}
	registry.StartAll(context.Background())
	if info := pluginState(t, registry, "a-broken"); info.State != PluginFailed || !strings.Contains(info.Error, "startup failed") || info.Failures != 1 || info.LastFailureAt == nil {
		t.Fatalf("failed plugin = %+v", info)
	}
	if info := pluginState(t, registry, "b-panics"); info.State != PluginFailed || !strings.Contains(info.Error, "panic: boom") {
		t.Fatalf("panicking plugin = %+v", info)
	}
	if info := pluginState(t, registry, "c-healthy"); info.State != PluginRunning {
		t.Fatalf("healthy plugin = %+v", info)
	}
	failed := 0
	for len(stream) > 0 {
		if e := <-stream; e.Type == "plugin.failed" {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("plugin.failed events = %d, want 2", failed)
	}
}

// Disabling persists across registries and stops the plugin now; enabling starts it again.
func TestPluginEnablementPersistsAndAppliesLive(t *testing.T) {
	registry, _, statePath := newTestRegistry(t)
	starts, stops := 0, 0
	probe := &pluginProbe{manifest: probeManifest("toggled"),
		start: func(context.Context, pluginapi.Host) error { starts++; return nil },
		stop:  func(context.Context) error { stops++; return nil }}
	if err := registry.Register(probe, false); err != nil {
		t.Fatal(err)
	}
	registry.StartAll(context.Background())
	ctx := context.Background()
	if err := registry.SetEnabled(ctx, "toggled", false); err != nil {
		t.Fatal(err)
	}
	if info := pluginState(t, registry, "toggled"); info.State != PluginDisabled || info.Enabled || stops != 1 {
		t.Fatalf("after disable: %+v stops=%d", info, stops)
	}
	if err := registry.SetEnabled(ctx, "missing", true); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("unknown plugin error = %v", err)
	}

	next := newPluginRegistry(newEventBus(), newHookDispatcher(nil), statePath)
	if err := next.Register(probe, false); err != nil {
		t.Fatal(err)
	}
	next.StartAll(ctx)
	if info := pluginState(t, next, "toggled"); info.State != PluginDisabled || starts != 1 {
		t.Fatalf("disabled plugin started on the next open: %+v starts=%d", info, starts)
	}
	if err := next.SetEnabled(ctx, "toggled", true); err != nil {
		t.Fatal(err)
	}
	if info := pluginState(t, next, "toggled"); info.State != PluginRunning || !info.Enabled || starts != 2 {
		t.Fatalf("after enable: %+v starts=%d", info, starts)
	}
}

// An unreadable enablement file never blocks startup, and is never overwritten.
func TestPluginStateFileErrorsAreReportedNotFatal(t *testing.T) {
	registry, _, statePath := newTestRegistry(t)
	if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&pluginProbe{manifest: probeManifest("p")}, false); err != nil {
		t.Fatal(err)
	}
	registry.StartAll(context.Background())
	catalog := registry.Catalog()
	if catalog.StateError == "" || catalog.Plugins[0].State != PluginRunning {
		t.Fatalf("catalog = %+v", catalog)
	}
	if err := registry.SetEnabled(context.Background(), "p", false); !errors.Is(err, ErrPluginStateInvalid) {
		t.Fatalf("SetEnabled over an invalid file = %v", err)
	}
	if raw, _ := os.ReadFile(statePath); string(raw) != "{not json" {
		t.Fatal("invalid state file was overwritten")
	}
}

func TestHookOrderRefusalAndFailurePolicies(t *testing.T) {
	registry, events, _ := newTestRegistry(t)
	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	register := func(id string, hook pluginapi.BeforeSessionImportHook) {
		t.Helper()
		if err := registry.Register(&pluginProbe{manifest: probeManifest(id, pluginapi.CapabilityBeforeSessionImport), start: func(_ context.Context, host pluginapi.Host) error {
			return host.RegisterBeforeSessionImportHook(hook)
		}}, false); err != nil {
			t.Fatal(err)
		}
	}
	register("a-slow", pluginapi.BeforeSessionImportHook{Priority: 1, Timeout: 5 * time.Millisecond, Policy: pluginapi.HookFailOpen,
		Handle: func(ctx context.Context, _ pluginapi.BeforeSessionImportRequest) error {
			record("slow")
			<-ctx.Done()
			return ctx.Err()
		}})
	register("b-panics", pluginapi.BeforeSessionImportHook{Priority: 2, Policy: pluginapi.HookFailOpen,
		Handle: func(context.Context, pluginapi.BeforeSessionImportRequest) error {
			record("panic")
			panic("hook bug")
		}})
	register("c-refuses", pluginapi.BeforeSessionImportHook{Priority: 3, Policy: pluginapi.HookFailClosed,
		Handle: func(context.Context, pluginapi.BeforeSessionImportRequest) error {
			record("refuse")
			return errors.New("policy says no")
		}})
	registry.StartAll(context.Background())
	stream, cancel := events.subscribe(16)
	defer cancel()

	err := registry.hooks.beforeSessionImport(context.Background(), pluginapi.BeforeSessionImportRequest{Source: "omp"})
	if err == nil || !strings.Contains(err.Error(), "policy says no") {
		t.Fatalf("fail-closed refusal = %v", err)
	}
	if want := []string{"slow", "panic", "refuse"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("hook order = %v, want %v", order, want)
	}
	// The timeout is a failure the plugin survives; the panic fails its plugin and drops its hook;
	// the refusal is a decision and no failure.
	if info := pluginState(t, registry, "a-slow"); info.State != PluginRunning || info.Failures != 1 {
		t.Fatalf("timed-out hook plugin = %+v", info)
	}
	if info := pluginState(t, registry, "b-panics"); info.State != PluginFailed {
		t.Fatalf("panicking hook plugin = %+v", info)
	}
	if info := pluginState(t, registry, "c-refuses"); info.State != PluginRunning || info.Failures != 0 {
		t.Fatalf("refusing hook plugin = %+v", info)
	}
	hookFailures := 0
	for len(stream) > 0 {
		if e := <-stream; e.Type == "plugin.hook.failed" {
			hookFailures++
		}
	}
	if hookFailures != 2 {
		t.Fatalf("plugin.hook.failed events = %d, want 2", hookFailures)
	}
	order = nil
	_ = registry.hooks.beforeSessionImport(context.Background(), pluginapi.BeforeSessionImportRequest{})
	if want := []string{"slow", "refuse"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("failed plugin's hook still runs: %v", order)
	}
	if err := registry.SetEnabled(context.Background(), "c-refuses", false); err != nil {
		t.Fatal(err)
	}
	if err := registry.hooks.beforeSessionImport(context.Background(), pluginapi.BeforeSessionImportRequest{}); err != nil {
		t.Fatalf("disabled plugin's hook still refuses: %v", err)
	}
}

func TestPluginHostRequiresDeclaredCapabilities(t *testing.T) {
	registry, _, _ := newTestRegistry(t)
	var subscribeErr, hookErr error
	if err := registry.Register(&pluginProbe{manifest: probeManifest("limited"), start: func(_ context.Context, host pluginapi.Host) error {
		_, _, subscribeErr = host.SubscribeEvents(1)
		hookErr = host.RegisterBeforeSessionImportHook(pluginapi.BeforeSessionImportHook{Policy: pluginapi.HookFailClosed,
			Handle: func(context.Context, pluginapi.BeforeSessionImportRequest) error { return nil }})
		return nil
	}}, false); err != nil {
		t.Fatal(err)
	}
	registry.StartAll(context.Background())
	if subscribeErr == nil || hookErr == nil {
		t.Fatalf("undeclared capabilities were granted: subscribe=%v hook=%v", subscribeErr, hookErr)
	}
}

func TestPluginEventListenerStreamClosesWhenPluginStops(t *testing.T) {
	registry, events, _ := newTestRegistry(t)
	var stream <-chan Event
	if err := registry.Register(&pluginProbe{manifest: probeManifest("observer", pluginapi.CapabilityEvents), start: func(_ context.Context, host pluginapi.Host) error {
		var err error
		stream, _, err = host.SubscribeEvents(4)
		return err
	}}, false); err != nil {
		t.Fatal(err)
	}
	registry.StartAll(context.Background())
	events.publish("session.imported", "s1", map[string]any{"source": "omp"})
	select {
	case e := <-stream:
		if e.Type != "session.imported" || e.SessionID != "s1" {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not receive the event")
	}
	if err := registry.SetEnabled(context.Background(), "observer", false); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("stream stayed open after the plugin was disabled")
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not close after the plugin was disabled")
	}
}

// importBlockerPlugin refuses every import with message.
type importBlockerPlugin struct{ message string }

func (p *importBlockerPlugin) Manifest() pluginapi.Manifest {
	return probeManifest("import-blocker", pluginapi.CapabilityBeforeSessionImport)
}
func (p *importBlockerPlugin) Start(_ context.Context, host pluginapi.Host) error {
	return host.RegisterBeforeSessionImportHook(pluginapi.BeforeSessionImportHook{Priority: 10, Policy: pluginapi.HookFailClosed,
		Handle: func(context.Context, pluginapi.BeforeSessionImportRequest) error { return errors.New(p.message) }})
}
func (p *importBlockerPlugin) Stop(context.Context) error { return nil }

// fakeSource is a session import source whose bundle a test controls.
type fakeSource struct {
	id, source string
	bundle     *pluginapi.ImportBundle
	sessions   []pluginapi.ImportSourceSession
	panicLoad  bool
	loads      int
}

func (f *fakeSource) Manifest() pluginapi.Manifest {
	return probeManifest(f.id, pluginapi.CapabilitySessionImport)
}
func (f *fakeSource) Start(context.Context, pluginapi.Host) error { return nil }
func (f *fakeSource) Stop(context.Context) error                  { return nil }
func (f *fakeSource) ImportSource() pluginapi.ImportSourceInfo {
	return pluginapi.ImportSourceInfo{ID: f.source, Name: f.source, Converter: 1}
}
func (f *fakeSource) ListSessions(context.Context, pluginapi.ImportListRequest) ([]pluginapi.ImportSourceSession, error) {
	return f.sessions, nil
}
func (f *fakeSource) LoadSession(context.Context, pluginapi.ImportLoadRequest) (*pluginapi.ImportBundle, error) {
	f.loads++
	if f.panicLoad {
		panic("adapter bug")
	}
	return f.bundle, nil
}
