package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func listenAddress() (host, port string, err error) {
	host, port, err = net.SplitHostPort(env("PLANO_ADDR", ":8080"))
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil {
		return "", "", errors.New("PLANO_ADDR must specify a host and numeric port, or :port")
	}
	return host, port, nil
}

func healthEndpoint() (string, error) {
	if endpoint, err := healthOverride(); endpoint != "" || err != nil {
		return endpoint, err
	}
	host, port, err := listenAddress()
	if err != nil {
		return "", err
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/healthz"}
	return endpoint.String(), nil
}

func healthOverride() (string, error) {
	endpoint := os.Getenv("PLANO_HEALTH_URL")
	if endpoint == "" {
		return "", nil
	}
	invalid := errors.New("PLANO_HEALTH_URL must be a valid HTTP(S) probe URL without credentials or a fragment")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || strings.Contains(endpoint, "#") || strings.Contains(parsed.RawQuery, " ") ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || strings.HasSuffix(parsed.Host, ":") {
		return "", invalid
	}
	host := parsed.Hostname()
	if host == "" || strings.ContainsAny(host, "[]") {
		return "", invalid
	}
	if strings.Contains(host, ":") || strings.HasPrefix(parsed.Host, "[") {
		address, err := netip.ParseAddr(host)
		if err != nil || !address.Is6() || !strings.HasPrefix(parsed.Host, "[") {
			return "", invalid
		}
	}
	if value := parsed.Port(); value != "" {
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			return "", invalid
		}
	}
	return endpoint, nil
}

func checkHealth(endpoint string, transport http.RoundTripper) error {
	if transport == nil {
		direct := http.DefaultTransport.(*http.Transport).Clone()
		direct.Proxy = nil
		transport = direct
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("Plano health endpoint did not return HTTP 200")
	}
	const maxHealthBytes = 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBytes+1))
	if err != nil {
		return err
	}
	var health struct {
		Status string `json:"status"`
		Mode   string `json:"mode"`
	}
	if len(body) > maxHealthBytes || json.Unmarshal(body, &health) != nil || health.Status != "ok" ||
		(health.Mode != "demo" && health.Mode != "live") {
		return errors.New("invalid Plano health response")
	}
	return nil
}
