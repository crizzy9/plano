# Plano

Plano is a self-hosted flight tracker with an Expo mobile client and a Go backend. It keeps a flight's status, inbound aircraft, telemetry and travel history together, without paid tiers.

```sh
docker compose up --build --wait
curl --fail http://localhost:8080/healthz
```

Demo mode starts with synthetic flights and requires no credentials. Search for `UA123` or `UA456` by date. Use the mobile app's server setting to connect to your own HTTPS origin.

- [Self-hosting and credentials](docs/SELF_HOSTING.md)
- [Architecture and Flighty evidence](ARCHITECTURE.md)
- [Mobile app](mobile/)

Run backend checks with Go 1.26 and a C compiler using `CGO_ENABLED=1 go test ./...`.
