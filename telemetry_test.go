package telemetry

import (
	"context"
	"testing"
)

// The whole value of the zero value is that a service can call Start
// unconditionally at the top of main. If an empty endpoint returned an error, or
// a nil Telemetry, every caller would need a branch — and the branch not taken
// is the one that rots.
func TestNoEndpointIsInertNotAnError(t *testing.T) {
	tel, err := Start(Config{AppName: "svc"})
	if err != nil {
		t.Fatalf("empty endpoint returned an error: %v", err)
	}
	if tel == nil {
		t.Fatal("Start returned nil; callers must be able to defer Shutdown unconditionally")
	}
	if tel.Enabled() {
		t.Fatal("Enabled() is true with no endpoint")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on a disabled Telemetry: %v", err)
	}
}

// Shutdown runs on paths that may already have shut down (a failed boot, a
// second signal handler). Panicking there turns a clean exit into a crash.
func TestShutdownIsIdempotentAndNilSafe(t *testing.T) {
	var nilTel *Telemetry
	if err := nilTel.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on nil: %v", err)
	}
	tel, _ := Start(Config{AppName: "svc"})
	for i := 0; i < 3; i++ {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown #%d: %v", i+1, err)
		}
	}
}

// A missing AppName with a real endpoint is a config mistake worth failing on:
// every batch would arrive unattributable, which is worse than not arriving.
func TestEndpointWithoutAppNameIsRejected(t *testing.T) {
	if _, err := Start(Config{Endpoint: "127.0.0.1:4317"}); err == nil {
		t.Fatal("expected an error when Endpoint is set and AppName is not")
	}
}

// Endpoints are widely configured as URLs from the OTLP era. Passing one
// through dials a host named "http" — a baffling failure for a config that
// looks correct.
func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"o11y.hanzo.svc:4317":          "o11y.hanzo.svc:4317",
		"http://o11y.hanzo.svc:4317":   "o11y.hanzo.svc:4317",
		"https://o11y.hanzo.svc:4317":  "o11y.hanzo.svc:4317",
		"http://o11y.hanzo.svc:4317/":  "o11y.hanzo.svc:4317",
		"https://o11y.hanzo.svc/v1/tr": "o11y.hanzo.svc",
		"  o11y.hanzo.svc:4317  ":      "o11y.hanzo.svc:4317",
		"":                             "",
		"   ":                          "",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// Naming a subset must not quietly start the others: a service that asked for
// logs only should not find itself exporting metrics it never wanted.
func TestSignalSubsetIsHonored(t *testing.T) {
	tel, err := Start(Config{
		AppName:  "svc",
		Endpoint: "127.0.0.1:14317", // nothing listening; exporters connect lazily
		Signals:  []Signal{Logs},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tel.Shutdown(context.Background())

	if tel.lp == nil {
		t.Fatal("logs were requested but no logger provider was installed")
	}
	if tel.mp != nil {
		t.Fatal("a metric provider was installed though only logs were requested")
	}
	if tel.tracer != nil {
		t.Fatal("a tracer was installed though only logs were requested")
	}
}

// An unreachable collector must not fail Start. Exporters connect lazily and
// retry; a service that refuses to boot because telemetry is down has made
// observation a dependency of serving.
func TestUnreachableCollectorStillStarts(t *testing.T) {
	tel, err := Start(Config{
		AppName:  "svc",
		Endpoint: "127.0.0.1:14318",
	})
	if err != nil {
		t.Fatalf("unreachable collector failed Start: %v", err)
	}
	if !tel.Enabled() {
		t.Fatal("Enabled() is false though providers were installed")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
