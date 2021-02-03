# Build the manager binary
FROM golang:1.15 as builder

# Copy the rpk as a close depedency
WORKDIR /workspace
COPY rpk/ rpk/

WORKDIR /workspace/k8s
# Copy the Go Modules manifests
COPY k8s/go.mod go.mod
COPY k8s/go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY k8s/main.go main.go
COPY k8s/apis/ apis/
COPY k8s/controllers/ controllers/
COPY k8s/pkg/ pkg/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -a -o manager main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/k8s/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
