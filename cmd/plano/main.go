package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crizzy9/plano/internal/api"
	"github.com/crizzy9/plano/internal/core"
	"github.com/crizzy9/plano/internal/demodata"
	"github.com/crizzy9/plano/internal/feeds"
	"github.com/crizzy9/plano/internal/predict"
	"github.com/crizzy9/plano/internal/push"
)

func main() {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("PLANO_LOG_LEVEL", "info"))); err != nil {
		fmt.Fprintln(os.Stderr, "invalid PLANO_LOG_LEVEL")
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		endpoint, err := healthEndpoint()
		if err != nil || checkHealth(endpoint, nil) != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "export-delay-samples" {
		if err := core.ExportDelaySamples(context.Background(), env("PLANO_DB", "plano.db"), os.Stdout); err != nil {
			logger.Error("delay sample export failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		if len(os.Args) != 3 || os.Args[2] == "" {
			fmt.Fprintln(os.Stderr, "usage: plano backup <destination.db>")
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := core.Backup(ctx, env("PLANO_DB", "plano.db"), os.Args[2]); err != nil {
			logger.Error("backup failed", "error", err)
			os.Exit(1)
		}
		return
	}
	checkOnly := len(os.Args) == 2 && os.Args[1] == "check-config"
	if len(os.Args) > 1 && !checkOnly {
		fmt.Fprintln(os.Stderr, "usage: plano [check-config|healthcheck|backup <destination.db>|export-delay-samples]")
		os.Exit(2)
	}
	if err := run(logger, checkOnly); err != nil {
		logger.Error("plano stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, checkOnly bool) error {
	demo, err := flag("PLANO_DEMO", false)
	if err != nil {
		return err
	}
	publicURL, err := api.PublicOrigin(env("PLANO_PUBLIC_URL", "http://localhost:8080"))
	if err != nil {
		return errors.New("PLANO_PUBLIC_URL must be an HTTP(S) origin")
	}
	enrollment, err := secret("PLANO_ENROLLMENT_CODE", "PLANO_ENROLLMENT_CODE_FILE")
	if err != nil {
		return err
	}
	if len(enrollment) != 0 && (len(enrollment) < 16 || len(enrollment) > 128) {
		return errors.New("enrollment code must be 16 to 128 bytes")
	}
	if !demo {
		allowOpen, err := flag("PLANO_ALLOW_OPEN_ENROLLMENT", false)
		if err != nil {
			return err
		}
		if len(enrollment) == 0 {
			if !allowOpen {
				return errors.New("live mode requires PLANO_ENROLLMENT_CODE or PLANO_ENROLLMENT_CODE_FILE; private deployments can set PLANO_ALLOW_OPEN_ENROLLMENT=true")
			}
			logger.Warn("live enrollment is open; restrict network access")
		}
	}
	bookingKey, err := secret("", "PLANO_BOOKING_KEY_FILE")
	if err != nil {
		return err
	}
	if len(bookingKey) > 0 && len(bookingKey) < 32 {
		return errors.New("PLANO_BOOKING_KEY_FILE requires at least 32 bytes")
	}
	var connections *api.ConnectionRules
	if path := os.Getenv("PLANO_AIRPORT_TRANSFERS_FILE"); path != "" {
		file, err := os.Open(path)
		if err != nil {
			return errors.New("cannot open PLANO_AIRPORT_TRANSFERS_FILE")
		}
		connections, err = api.LoadConnectionRules(file)
		_ = file.Close()
		if err != nil {
			return fmt.Errorf("load airport transfer rules: %w", err)
		}
	}
	var aeroCallbackKey, advisoryIngressKey []byte
	if !demo {
		aeroCallbackKey, err = secret("", "PLANO_AEROALERTS_KEY_FILE")
		if err != nil {
			return err
		}
		if len(aeroCallbackKey) != 0 && len(aeroCallbackKey) < 32 {
			return errors.New("PLANO_AEROALERTS_KEY_FILE requires at least 32 bytes")
		}
		advisoryIngressKey, err = secret("", "PLANO_ADVISORY_INGRESS_KEY_FILE")
		if err != nil {
			return err
		}
		if len(advisoryIngressKey) != 0 && len(advisoryIngressKey) < 32 {
			return errors.New("PLANO_ADVISORY_INGRESS_KEY_FILE requires at least 32 bytes")
		}
	}
	var flights feeds.FlightSource
	var aeroAPI *feeds.AeroAPI
	var aeroSubscriptions *core.AeroSubscriptionOptions
	var advisories []feeds.AdvisorySource
	var receiver *feeds.ADSBReceiver
	var registry *feeds.FAARegistry
	var sender push.Sender
	var reservationSource feeds.ReservationSource
	var tripItSync *api.TripItSync
	var weather *core.WeatherCache
	if demo {
		flights = demodata.Source{}
	} else {
		key, err := secret("PLANO_AEROAPI_KEY", "PLANO_AEROAPI_KEY_FILE")
		if err != nil {
			return err
		}
		if len(key) == 0 {
			return errors.New("live mode requires PLANO_AEROAPI_KEY or PLANO_AEROAPI_KEY_FILE; enable PLANO_DEMO for synthetic data")
		}
		aeroAPI = feeds.NewAeroAPI(string(key), nil)
		flights = aeroAPI
		enableMETAR, err := flag("PLANO_METAR", false)
		if err != nil {
			return err
		}
		if enableMETAR {
			weather = core.NewWeatherCache(feeds.NewMETAR(nil))
		}
		enableSubscriptions, err := flag("PLANO_AERO_SUBSCRIPTIONS", false)
		if err != nil {
			return err
		}
		if enableSubscriptions {
			maxActive, err := strconv.Atoi(env("PLANO_AERO_SUBSCRIPTIONS_MAX", "10"))
			if err != nil {
				return errors.New("invalid PLANO_AERO_SUBSCRIPTIONS_MAX")
			}
			cadence, err := time.ParseDuration(env("PLANO_AERO_SUBSCRIPTIONS_INTERVAL", "5m"))
			if err != nil {
				return errors.New("invalid PLANO_AERO_SUBSCRIPTIONS_INTERVAL")
			}
			if len(aeroCallbackKey) == 0 || !strings.HasPrefix(publicURL, "https://") {
				return errors.New("PLANO_AERO_SUBSCRIPTIONS requires PLANO_AEROALERTS_KEY_FILE and an HTTPS PLANO_PUBLIC_URL")
			}
			aeroSubscriptions = &core.AeroSubscriptionOptions{
				CallbackURL: publicURL + "/v1/webhooks/aeroapi?token=" + url.QueryEscape(string(aeroCallbackKey)),
				MaxActive:   maxActive,
				Interval:    cadence,
			}
			if err := aeroSubscriptions.Validate(); err != nil {
				return err
			}
		}
		tripItKey, err := secret("", "PLANO_TRIPIT_SYNC_KEY_FILE")
		if err != nil {
			return err
		}
		if len(tripItKey) != 0 {
			cadence, err := time.ParseDuration(env("PLANO_TRIPIT_SYNC_INTERVAL", "30m"))
			if err != nil {
				return errors.New("invalid PLANO_TRIPIT_SYNC_INTERVAL")
			}
			tripItSync, err = api.NewTripItSync(tripItKey, cadence)
			if err != nil {
				return err
			}
		}
		if endpoint := os.Getenv("PLANO_RESERVATION_GATEWAY_URL"); endpoint != "" {
			token, err := secret("PLANO_RESERVATION_GATEWAY_TOKEN", "PLANO_RESERVATION_GATEWAY_TOKEN_FILE")
			if err != nil {
				return err
			}
			reservationSource, err = feeds.NewReservationGateway(endpoint, string(token), nil)
			if err != nil {
				return err
			}
		}
		enableFAA, err := flag("PLANO_FAA_NAS", true)
		if err != nil {
			return err
		}
		if enableFAA {
			advisories = append(advisories, feeds.NewFAANAS(nil))
		}
		for _, gateway := range []struct{ prefix, source string }{{"PLANO_SWIM", "faa_swim"}, {"PLANO_EUROCONTROL", "eurocontrol_nm"}} {
			if endpoint := os.Getenv(gateway.prefix + "_URL"); endpoint != "" {
				token, err := secret(gateway.prefix+"_TOKEN", gateway.prefix+"_TOKEN_FILE")
				if err != nil {
					return err
				}
				client, err := feeds.NewAdvisoryGateway(endpoint, string(token), gateway.source, nil)
				if err != nil {
					return fmt.Errorf("%s advisory gateway configuration: %w", gateway.source, err)
				}
				advisories = append(advisories, client)
			}
		}
		if endpoint := os.Getenv("PLANO_ADSB_URL"); endpoint != "" {
			receiver, err = feeds.NewADSBReceiver(endpoint, nil)
			if err != nil {
				return err
			}
		}
		if path := os.Getenv("PLANO_FAA_REGISTRY_FILE"); path != "" {
			file, err := os.Open(path)
			if err != nil {
				return errors.New("cannot open PLANO_FAA_REGISTRY_FILE")
			}
			registry, err = feeds.LoadFAARegistry(file)
			_ = file.Close()
			if err != nil {
				return fmt.Errorf("load FAA registry: %w", err)
			}
		}
		sender, err = pushConfig()
		if err != nil {
			return err
		}
	}
	interval, err := time.ParseDuration(env("PLANO_REFRESH_INTERVAL", "60s"))
	if err != nil || interval < 5*time.Second || interval > 30*time.Minute {
		return errors.New("PLANO_REFRESH_INTERVAL must be between 5s and 30m")
	}
	var radarCadence time.Duration
	if receiver != nil {
		radarCadence, err = time.ParseDuration(env("PLANO_ADSB_REFRESH_INTERVAL", "5s"))
		if err != nil || radarCadence < 3*time.Second || radarCadence > time.Minute {
			return errors.New("PLANO_ADSB_REFRESH_INTERVAL must be between 3s and 60s")
		}
	}
	var delayModel *predict.Model
	if path := os.Getenv("PLANO_DELAY_MODEL_FILE"); path != "" && !demo {
		file, err := os.Open(path)
		if err != nil {
			return errors.New("cannot open PLANO_DELAY_MODEL_FILE")
		}
		delayModel, err = predict.Load(file)
		_ = file.Close()
		if err != nil {
			return fmt.Errorf("load delay model: %w", err)
		}
	}
	collectSamples, err := flag("PLANO_COLLECT_DELAY_SAMPLES", false)
	if err != nil {
		return err
	}
	host, port, err := listenAddress()
	if err != nil {
		return err
	}
	if checkOnly {
		logger.Info("plano configuration valid", "demo", demo, "apns", sender.APNs != nil, "fcm", sender.FCM != nil)
		return nil
	}
	store, err := core.OpenWithOptions(env("PLANO_DB", "plano.db"), core.StoreOptions{DelayModel: delayModel, CollectDelaySamples: collectSamples && !demo})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	service := api.Server{
		Store: store, Source: flights, PublicURL: publicURL, EnrollmentCode: string(enrollment),
		BookingKey: bookingKey, ReservationSource: reservationSource, TripItSync: tripItSync, ConnectionRules: connections, Weather: weather,
		AeroCallbackKey: aeroCallbackKey, AdvisoryIngressKey: advisoryIngressKey, Demo: demo, Logger: logger,
	}
	server := &http.Server{Addr: net.JoinHostPort(host, port), Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer stop()
	workers.Go(func() {
		(core.Refresher{Store: store, Flights: flights, Advisories: advisories, Registry: registry, Logger: logger, Interval: interval}).Run(ctx)
	})
	workers.Go(func() { service.RunConnectionMonitors(ctx) })
	if receiver != nil {
		workers.Go(func() { store.RunRadar(ctx, receiver, registry, radarCadence, logger) })
	}
	if sender.APNs != nil || sender.FCM != nil {
		workers.Go(func() { store.RunDispatcher(ctx, sender, logger) })
	}
	if len(aeroCallbackKey) != 0 {
		workers.Go(func() { store.RunAeroRefresh(ctx, flights, logger) })
	}
	if aeroSubscriptions != nil {
		workers.Go(func() { store.RunAeroSubscriptions(ctx, aeroAPI, *aeroSubscriptions, logger) })
	}
	if tripItSync != nil {
		workers.Go(func() { service.RunTripItSync(ctx) })
	}
	if weather != nil {
		workers.Go(func() { weather.Run(ctx, store, logger) })
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("plano starting", "addr", server.Addr, "demo", demo, "apns", sender.APNs != nil, "fcm", sender.FCM != nil)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func flag(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	flag, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return flag, nil
}

func secret(name, fileName string) ([]byte, error) {
	value, path := os.Getenv(name), os.Getenv(fileName)
	if path == "" {
		return []byte(strings.TrimSpace(value)), nil
	}
	if value != "" {
		return nil, fmt.Errorf("set only one of %s and %s", name, fileName)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", fileName)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, fmt.Errorf("invalid secret file for %s", fileName)
	}
	value = strings.TrimSpace(string(data))
	if value == "" {
		return nil, fmt.Errorf("empty secret file for %s", fileName)
	}
	return []byte(value), nil
}
