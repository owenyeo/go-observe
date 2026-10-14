# Local Grafana preview

Use the disposable compose stack. It binds ports to loopback and uses example-only
credentials (`admin` / `local-observe-demo`). Never reuse that password elsewhere.

```sh
export GOTOOLCHAIN="go$(cat .go-version)"
docker compose -p go-observe-preview -f examples/local/compose.yml up -d --wait
OBSERVE_TEST_GRAFANA=http://127.0.0.1:8793 go test ./grafana -run TestLocalGrafana -v
```

The integration test creates/removes its own local identity and folder. It verifies
dashboard + paused alert creation, unchanged publication and a subsequent update.

To inspect the example dashboard yourself:

1. Run `go run ./examples/service` and make requests to `/work` (including failures).
2. Open <http://127.0.0.1:8793> and sign in using the local example credentials.
3. Create a folder with UID `checkout` and a local service-account token with edit
   permissions. Set `GRAFANA_TOKEN` privately in your terminal environment.
4. Render the example bundle and run `observe diff`, then `observe apply`, adding
   `-allow-insecure` for this loopback HTTP instance. Rules remain paused.
5. Open the Checkout dashboard. Prometheus discovers the sample at
   `host.docker.internal:9090`; Linux Docker users need host networking or an
   explicit reachable sample endpoint in `prometheus.yml`.

The first samples do not demonstrate 30-day compliance. The dashboard needs traffic
and at least two scrapes for counter rates; stopped services should show missing
coverage rather than artificial healthy values.

```sh
go run ./examples/monitoring -version "$(cat examples/monitoring/VERSION)" | go run ./cmd/observe normalize -out artifacts/staging.json
go run ./cmd/observe diff -bundle artifacts/staging.json -allow-insecure
go run ./cmd/observe apply -bundle artifacts/staging.json -allow-insecure
go run ./examples/promql-check -out artifacts/promql-test.json
docker compose -p go-observe-preview -f examples/local/compose.yml run --rm --no-deps -T \
  -v "$PWD/artifacts:/checks:ro" --entrypoint /bin/promtool prometheus test rules /checks/promql-test.json
docker compose -p go-observe-preview -f examples/local/compose.yml down
```
