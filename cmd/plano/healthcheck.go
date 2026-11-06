package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	if endpoint := os.Getenv("PLANO_HEALTH_URL"); endpoint != "" {
		return endpoint, nil
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
