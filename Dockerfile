# syntax=docker/dockerfile:1.7
FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock-control-plane ./mock-control-plane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mock-control-plane /mock-control-plane
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/mock-control-plane"]
