# Monitor gotochanger via Prometheus and Grafana

You want an existing Prometheus server to scrape library metrics (slot/reader/volume counts, operation
throughput and latency, error rates) and a Grafana dashboard to visualize them.

## Prerequisites

- A Prometheus server that can reach the gotochangerd host's HTTP listener.
- A Grafana instance with a Prometheus data source configured (or you'll add one during import).
- Admin access to gotochangerd.

## Steps

1. Enable the exporter:
   ```sh
   gotochangerctl prometheus enable
   ```
   (Or from the web UI: Admin > Settings > "Prometheus" > enable the checkbox > Save.)
2. Confirm it's serving real values - no credentials needed:
   ```sh
   curl http://localhost:8480/metrics
   ```
3. Point your Prometheus server at it:
   ```yaml
   scrape_configs:
     - job_name: 'gotochanger'
       static_configs:
         - targets: ['<gotochangerd-host>:8480']
   ```
   Reload/restart Prometheus to pick up the new scrape config.
4. Download the pre-built Grafana dashboard:
   ```sh
   gotochangerctl prometheus dashboard gotochanger-dashboard.json
   ```
   (Or click "Download Grafana dashboard" from the same Admin > Settings > Prometheus panel.)
5. In Grafana: Dashboards > New > Import > upload `gotochanger-dashboard.json` > when prompted, select the
   Prometheus data source scraping this gotochangerd instance.
6. Generate some activity to see the dashboard populate - a load/unload cycle is the simplest:
   ```sh
   gotochangerctl load slot 1 0
   gotochangerctl unload 0 slot 1
   ```

## Verify

The imported dashboard's Overview row should show your real slot/reader/volume counts within one Prometheus
scrape interval. After the load/unload cycle in step 6, the Operations Timeline row's rate graph should show
a `load` and an `unload` sample, and the latency panel a non-zero p95 for both. If a panel shows "No data,"
double-check the data source selected during import matches the one actually scraping this instance - the
dashboard's queries assume metric names exactly as gotochangerd exports them (`gotochanger_*`), so a
differently-labeled or relabeled scrape job will need its queries adjusted.

Metrics reflect activity from *any* client - the web UI, `gotochangerctl`/`gotochanger-changer` over the
trusted socket, or direct API calls - so a real Bareos backup job driving this daemon shows up here exactly
like the manual load/unload above did.
