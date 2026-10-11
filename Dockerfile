# One image for every platform component: /api (serves the dashboard too),
# /controller and /migrate. Pinned bases; built multi-arch by
# .github/workflows/build-images.yml.

FROM node:24.10-alpine3.22 AS web
WORKDIR /web
COPY apps/web/package.json apps/web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY apps/web/ ./
RUN npm run build

FROM golang:1.25-alpine3.22 AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY db ./db
ARG TARGETOS TARGETARCH
RUN for c in api controller migrate; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/$c ./cmd/$c; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=go /out/ /
COPY --from=web /web/out /web
ENV WEB_DIR=/web
USER 65532:65532
ENTRYPOINT ["/api"]
