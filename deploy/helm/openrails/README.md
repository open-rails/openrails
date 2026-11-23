# OpenRails Helm chart

Runs the standalone OpenRails server (`openrails run-server`) as one Deployment.
PostgreSQL 18+ and Redis are yours: the chart takes their addresses in `config`
and their credentials from existing Secrets, and creates none.

```bash
helm install openrails oci://ghcr.io/open-rails/charts/openrails --version X.Y.Z \
  --namespace openrails --create-namespace -f values.yaml
```

Chart version `X.Y.Z` runs image `vX.Y.Z`. [`values.yaml`](values.yaml) documents
each value. Every value, a minimal `values.yaml`, secrets, probes, scaling,
upgrades and backups: [Kubernetes and Helm](../../../docs/standalone.md#kubernetes-and-helm).
The server's own configuration keys: [configuration reference](../../../docs/standalone.md#configuration-reference).
