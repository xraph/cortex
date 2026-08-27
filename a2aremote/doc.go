// Package a2aremote carries cortex messages over the Agent2Agent
// protocol, in both directions: remote A2A clients can address cortex
// agents, and cortex agents can address agents that are not cortex
// agents at all.
//
// It is a separate module from the root deliberately. gRPC pulls in
// grpc-go and protobuf, and a host that only ever wanted in-process
// messaging should not inherit that dependency graph.
//
// The wire shapes here follow A2A 1.0.0, whose canonical data model is
// specification/a2a.proto in github.com/a2aproject/A2A. Field names on
// the wire are the lowerCamelCase of the proto's field names, method
// names are PascalCase, and agent cards live at
// /.well-known/agent-card.json. The 0.x spellings (message/send,
// /.well-known/agent.json) are not served: a 1.0 client never asks for
// them.
//
// Every semantic decision lives in Service. The bindings translate
// formats and nothing else, which is what lets three of them share one
// implementation and, more importantly, one set of security rules.
package a2aremote
