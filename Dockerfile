# Build the orchestrator binary and run it in a minimal, non-root image.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/orchestrator ./cmd/orchestrator

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/orchestrator /orchestrator
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/orchestrator"]
