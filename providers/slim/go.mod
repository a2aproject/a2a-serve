module github.com/a2aproject/a2a-serve/providers/slim

go 1.26.6

replace github.com/a2aproject/a2a-serve => ../..

require (
	github.com/a2aproject/a2a-go/v2 v2.7.1-0.20261106135701-196d4bbfa1df
	github.com/a2aproject/a2a-serve v0.0.0-00010101000000-000000000000
	github.com/agntcy/slim-a2a-go v0.5.2
	github.com/agntcy/slim-bindings-go/v3 v3.3.9
	github.com/google/go-cmp v0.7.0
	golang.org/x/sys v0.49.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261012220534-4341edf3f941 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261026165753-2ffab32bce7e // indirect
	google.golang.org/grpc v1.85.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/agntcy/slim-a2a-go => ./third_party/slim-a2a-go
