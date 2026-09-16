# Build environment
# -----------------
FROM golang:1.25.6-bookworm AS builder
LABEL stage=builder

ARG DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# chart-inspector local-replaces its sibling modules (../composition-dynamic-controller,
# ../core-provider), so the build context is the go/ workspace: copy the whole tree so the
# replaces resolve, then build the chart-inspector module.
WORKDIR /src
COPY . .
WORKDIR /src/chart-inspector
RUN go mod download
# Build stamp (#105). These come from build_args in release-{tag,pullrequest}.yaml; an unset ARG
# leaves main.go's "dev"/"unknown" defaults, so a local `docker build` is honestly labelled rather
# than claiming to be a release.
ARG VERSION
ARG COMMIT_HASH
RUN CGO_ENABLED=0 GO111MODULE=on go build -a \
      -ldflags "-X main.buildVersion=${VERSION:-dev} -X main.buildCommit=${COMMIT_HASH:-unknown}" \
      -o /bin/webservice ./main.go && strip /bin/webservice

# Deployment environment
# ----------------------
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/webservice /bin/webservice
USER nonroot:nonroot
ENTRYPOINT ["/bin/webservice"]
