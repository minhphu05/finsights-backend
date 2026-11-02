FROM golang:1.26-alpine AS build

RUN apk add --no-cache protobuf
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .

ARG TARGETARCH
ARG APP
RUN test -n "$APP" && \
    GOBIN=/usr/local/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 && \
    GOBIN=/usr/local/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1 && \
    protoc -I proto -I /usr/include --go_out=. --go_opt=module=github.com/minhphu05/finsights-backend \
      --go-grpc_out=. --go-grpc_opt=module=github.com/minhphu05/finsights-backend $(find proto -name '*.proto' -print) && \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/$APP

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
USER 65532:65532
ENTRYPOINT ["/service"]
