## Summary

Describe the user-visible change and why it is needed.

## Validation

- [ ] `go test -race ./...`
- [ ] `go vet ./...`
- [ ] Docker/Compose changes were rendered or smoke-tested
- [ ] Helm changes pass `helm lint` and `helm template`
- [ ] Dashboard and Prometheus rule changes were validated

## Exporter compatibility

- [ ] The change only uses read-only operational or business-statistics APIs
- [ ] Metric names, types, labels and stale-data behavior remain compatible, or the breaking change is documented
- [ ] API calls still pass through the configured limiter, retry budget and scheduler
- [ ] No AK/SK, token, production resource name or account data is committed
