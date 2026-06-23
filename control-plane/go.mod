module github.com/dlddu/session-platform/control-plane

go 1.24

// NOTE(scaffolding): The control plane currently builds on the Go standard
// library only, so `make build` works without any module downloads. Real
// adapter implementations will introduce external dependencies here, e.g.:
//
//   require (
//     k8s.io/client-go v0.30.x   // PodOrchestrator — data plane pod lifecycle
//     github.com/redis/go-redis/v9 v9.x // StateStore — atomic state transitions
//   )
//
// They are intentionally omitted while the adapters are in-memory stubs.
