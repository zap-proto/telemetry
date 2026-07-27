// Package telemetry starts a service's traces, metrics and logs on the ZAP
// wire in one call.
//
// The three signals already share everything that matters: the same envelope,
// the same port, the same node handshake, and the same convention of carrying
// the message type in the header flags' high byte. What they did not share was
// a way to be turned on. Each service dialed three times, configured three
// exporters, and installed three providers — three chances to get two right and
// one wrong, silently, because a provider that was never installed discards
// every measurement while the code still reads as instrumented.
//
//	tel, err := telemetry.Start(telemetry.Config{
//	    AppName:  "iam",
//	    Endpoint: "o11y.hanzo.svc:4317",
//	})
//	if err != nil { return err }
//	defer tel.Shutdown(ctx)
//
// # Noop is the zero value
//
// An empty Endpoint means the whole thing is inert: nothing dials, no provider
// is installed, and the OTel globals stay as they were. That is what makes it
// safe to call unconditionally at the top of main — a service picks up
// telemetry when its deployment supplies an endpoint, and costs nothing when it
// does not. There is no separate "disabled" path to keep working.
//
// # Not the event bus
//
// Telemetry is observation: fire-and-forget, lossy-tolerable, and it never
// affects business state. Events are facts — a payment settled, an org was
// created — which are durable, ordered and acted upon. Routing spans through
// the event bus makes telemetry volume compete with money-path delivery and
// both get worse. Facts go to the event bus; this goes to o11y.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	luxlog "github.com/luxfi/log"
	luxmetric "github.com/luxfi/metric"
	luxtrace "github.com/luxfi/trace"
)

// Signal names a telemetry stream.
type Signal string

const (
	Traces  Signal = "traces"
	Metrics Signal = "metrics"
	Logs    Signal = "logs"
)

// Config describes what to start.
type Config struct {
	// AppName is the service name on every emitted batch. Required unless the
	// Endpoint is empty.
	AppName string

	// Version is reported alongside AppName.
	Version string

	// Endpoint is the collector address as host:port. Empty ⇒ everything is a
	// noop. A scheme or path is trimmed: they have no meaning on this wire, and
	// silently dialing "https" as a hostname is a worse failure than ignoring it.
	Endpoint string

	// Resource attributes attached to every batch (k8s namespace, pod, region…).
	Resource map[string]string

	// Signals to start. Empty ⇒ all three. Naming a subset is for a service that
	// genuinely has nothing to say on one of them, not for turning telemetry
	// down: an unwanted signal costs a provider and no traffic.
	Signals []Signal

	// SampleRate for traces, 0..1. Zero ⇒ 1 (sample everything). A service that
	// wants no traces should leave Traces out of Signals rather than set 0 here,
	// so the intent is visible.
	SampleRate float64
}

// Telemetry owns what Start installed.
type Telemetry struct {
	tracer luxtrace.Tracer
	mp     *sdkmetric.MeterProvider
	lp     *sdklog.LoggerProvider

	enabled bool
}

// Enabled reports whether anything was actually started. A caller that logs its
// telemetry state should read this rather than assume Start succeeded — the
// no-endpoint case returns a valid, inert Telemetry and a nil error.
func (t *Telemetry) Enabled() bool { return t != nil && t.enabled }

// Start installs the providers for the configured signals.
//
// It returns a usable Telemetry even when disabled, so callers never need a nil
// check before Shutdown.
func Start(cfg Config) (*Telemetry, error) {
	endpoint := normalizeEndpoint(cfg.Endpoint)
	if endpoint == "" {
		return &Telemetry{}, nil
	}
	if cfg.AppName == "" {
		return &Telemetry{}, errors.New("telemetry: AppName is required when Endpoint is set")
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 1
	}

	want := map[Signal]bool{}
	if len(cfg.Signals) == 0 {
		want[Traces], want[Metrics], want[Logs] = true, true, true
	} else {
		for _, s := range cfg.Signals {
			want[s] = true
		}
	}

	res := resource.NewSchemaless(resourceAttrs(cfg)...)
	t := &Telemetry{}

	// Each signal is attempted independently. One collector rejecting metrics
	// must not cost the service its traces — partial telemetry beats none, and
	// the failure is reported rather than hidden.
	var errs []error

	if want[Traces] {
		tracer, err := luxtrace.New(luxtrace.Config{
			ExporterConfig:  luxtrace.ExporterConfig{Type: luxtrace.ZAP, Endpoint: endpoint},
			TraceSampleRate: cfg.SampleRate,
			AppName:         cfg.AppName,
			Version:         cfg.Version,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("traces: %w", err))
		} else {
			t.tracer = tracer
			t.enabled = true
		}
	}

	if want[Metrics] {
		exp, err := luxmetric.NewOTelZAPExporter(luxmetric.ZAPExporterConfig{
			Endpoint: endpoint,
			AppName:  cfg.AppName,
			Version:  cfg.Version,
			Resource: cfg.Resource,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("metrics: %w", err))
		} else {
			mp := sdkmetric.NewMeterProvider(
				sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
				sdkmetric.WithResource(res),
			)
			otel.SetMeterProvider(mp)
			t.mp = mp
			t.enabled = true
		}
	}

	if want[Logs] {
		exp, err := luxlog.NewZAPExporter(luxlog.ZAPExporterConfig{
			Endpoint: endpoint,
			AppName:  cfg.AppName,
			Version:  cfg.Version,
			Resource: cfg.Resource,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("logs: %w", err))
		} else {
			lp := sdklog.NewLoggerProvider(
				sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
				sdklog.WithResource(res),
			)
			otellog.SetLoggerProvider(lp)
			t.lp = lp
			t.enabled = true
		}
	}

	// W3C trace context so a span links to the caller's trace across the edge,
	// giving one end-to-end trace rather than a per-service fragment.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if len(errs) > 0 {
		return t, errors.Join(errs...)
	}
	return t, nil
}

// Shutdown flushes and stops everything Start installed. Safe on a disabled
// Telemetry and safe to call twice.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.lp != nil {
		if err := t.lp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("logs: %w", err))
		}
		t.lp = nil
	}
	if t.mp != nil {
		if err := t.mp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("metrics: %w", err))
		}
		t.mp = nil
	}
	if t.tracer != nil {
		if err := t.tracer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("traces: %w", err))
		}
		t.tracer = nil
	}
	t.enabled = false
	return errors.Join(errs...)
}

func resourceAttrs(cfg Config) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(cfg.Resource)+2)
	attrs = append(attrs, attribute.String("service.name", cfg.AppName))
	if cfg.Version != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.Version))
	}
	for k, v := range cfg.Resource {
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}

// normalizeEndpoint reduces a configured value to host:port.
//
// Endpoints are widely configured as URLs from the OTLP era. A scheme and path
// mean nothing on this wire, and passing them through produces a dial against a
// hostname like "http" — a confusing failure for a config that looks right.
func normalizeEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ""
	}
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	if i := strings.IndexByte(ep, '/'); i >= 0 {
		ep = ep[:i]
	}
	return ep
}
