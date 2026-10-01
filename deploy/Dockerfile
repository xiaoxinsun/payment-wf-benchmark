# One image with every binary; compose picks the command. Built from the repo root (go.work needs all modules).
FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY . .
RUN go work edit -dropuse ./spikes/dbos -dropuse ./spikes/temporal
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    mkdir -p /out && \
    (cd engine-temporal && go build -trimpath -ldflags="-s -w" -o /out/payment-temporal ./cmd/payment-temporal) && \
    (cd engine-dbos && go build -trimpath -ldflags="-s -w" -o /out/payment-dbos ./cmd/payment-dbos) && \
    (cd sims && go build -trimpath -ldflags="-s -w" -o /out/sim-cbs ./cmd/sim-cbs && \
                go build -trimpath -ldflags="-s -w" -o /out/sim-aml ./cmd/sim-aml && \
                go build -trimpath -ldflags="-s -w" -o /out/sim-npc ./cmd/sim-npc)

FROM alpine:3.23
COPY --from=build /out/ /usr/local/bin/
COPY deploy/config /etc/ibps/config
ENV CONFIG_DIR=/etc/ibps/config
