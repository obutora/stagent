// Package emupb is the Go code of the Android emulator's gRPC service,
// generated from emulator_controller.proto. The .proto is vendored
// unchanged from the Android Emulator 37.2.12.0 SDK package
// (<sdk>/emulator/lib/emulator_controller.proto); the generated files are
// committed, so building stagent needs no protoc.
//
// Pinned generator: protoc 35.1 (libprotoc of grpcio-tools 1.84.0, run
// through uv), protoc-gen-go v1.36.12, protoc-gen-go-grpc v1.6.2. To update,
// copy the newer .proto over the vendored one and run `go generate` here
// (needs sh, uv and Go).
package emupb

//go:generate sh -c "GOBIN=\"$PWD/.bin\" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12"
//go:generate sh -c "GOBIN=\"$PWD/.bin\" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2"
//go:generate sh -c "uv run --no-project --with grpcio-tools==1.84.0 python -m grpc_tools.protoc -I. --plugin=protoc-gen-go=.bin/protoc-gen-go --plugin=protoc-gen-go-grpc=.bin/protoc-gen-go-grpc --go_out=. --go_opt=paths=source_relative,Memulator_controller.proto=github.com/obutora/stagent/internal/hostscreen/android/emupb --go-grpc_out=. --go-grpc_opt=paths=source_relative,Memulator_controller.proto=github.com/obutora/stagent/internal/hostscreen/android/emupb emulator_controller.proto"
//go:generate rm -rf .bin
