# NVIDIA OpenShell extension protos

Copied from https://github.com/NVIDIA/OpenShell at tag `v0.1.4`, `proto/`:
`compute_driver.proto` and its import closure (`extension.proto`,
`options.proto`, `sandbox.proto`, `datamodel.proto`). Licensed by NVIDIA under
Apache-2.0 (see `LICENSE`); the files keep their SPDX headers. One deviation:
`options.proto` omits the `google.protobuf.MethodOptions` extension
`authorization = 50000`, which collides with `ateapi.method` (same number,
same extended message) in `go.ate.dev/substrate`; the Go protobuf registry
panics on such a pair at init, and the compute-driver contract only uses the
`secret` field option (50001). Options are descriptor metadata, not wire
format, so the gateway is unaffected.

`gen/` holds the Go stubs produced from them with `buf generate` run in
`proto/` (protoc-gen-go v1.36.11, protoc-gen-go-grpc v1.5.1). Regenerate after
updating the protos to a new OpenShell release, and keep the driver's
`bootstrap.go` in step with that release's launch contract.
