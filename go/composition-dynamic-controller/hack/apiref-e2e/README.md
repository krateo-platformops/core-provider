# apiRef end-to-end test (real authn + real snowplow on kind)

Proves the CDC `apiRef` status source works against the **real** Krateo platform — not stubs:

```
projected SA token (file)
  ─ authn.Client.Token ─▶ authn POST /serviceaccount/login   (TokenReview → accessToken)
  ─ snowplow.Client.Resolve(Bearer JWT, extras) ─▶ snowplow GET /call   (resolves a RESTAction)
  ─▶ RESTAction echoes the request extras ─▶ .api.echo.args
```

The test exercises the actual CDC client code (`internal/authn`, `internal/snowplow`,
`internal/composition.SnowplowAPIResolver`) and asserts that both the **static** extras
(`region`, from the CompositionDefinition's `apiRef.extras`) and the **per-instance** extras
(`compositionName`/`compositionNamespace`/`compositionId`, injected by the resolver, request-wins)
round-trip through the authn-issued JWT and snowplow's RESTAction resolution.

## Run

```bash
# needs: docker, kind, kubectl, go; ../authn (main) and ../snowplow checkouts
hack/apiref-e2e/run.sh
```

`run.sh` creates a kind cluster, builds authn + snowplow:1.1.1 images from source, deploys them
(+ an in-cluster `go-httpbin` echo server) with a shared `JWT_SIGN_KEY`, applies the fixtures
(test ServiceAccount, its `serviceaccount.authn.krateo.io` allowlist mapping, the `status-sources`
RESTAction, group RBAC), mints an audience-`authn` token via TokenRequest, port-forwards both
services, and runs the tagged test:

```bash
go test -tags e2e ./internal/composition/ -run TestE2E_ApiRefChain -v
```

## What success looks like

- authn log: `serviceaccount auth succeeded  username=cdc-e2e  groups=krateo:cdc-e2e`
- snowplow log: `base dict for api resolver  dict={compositionId,compositionName,compositionNamespace,region}`
  then `RESTAction successfully resolved  name=status-sources`
- test: `.api.echo.args` = `{cn: demo-app, cns: apps, cid: uid-e2e-123, region: eu}`
