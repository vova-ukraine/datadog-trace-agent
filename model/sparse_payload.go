package model

import (
	pb "github.com/DataDog/datadog-trace-agent/model/protobuf"
	"github.com/golang/protobuf/proto"
	"sync"
)

// SparseAgentPayload is a sparse version of AgentPayload, ignoring StatsBuckets
type SparseAgentPayload struct {
	HostName     string                `json:"hostname"`     // the host name that will be resolved by the API
	Env          string                `json:"env"`          // the default environment this agent uses
	Transactions []AnalyzedTransaction `json:"transactions"` // unsampled traces, most will comprise of just root spans

	// private
	mu     sync.RWMutex
	extras map[string]string
}

// AnalyzedTransaction is a single analyzed transaction
type AnalyzedTransaction struct {
	Span
	Message string `json:"message"`
}

// ToProto converts a sparse agent payload to proto
func (p *SparseAgentPayload) ToProto() *pb.TracePayload {
	ts := make([]*pb.Trace, 0, len(p.Transactions))
	for range p.Transactions {
		ts = append(ts, nil)
	}

	return &pb.TracePayload{
		HostName: p.HostName,
		Env:      p.Env,
		Traces:   ts,
	}

}

// ToProtobufBytes converts a sparse agent payload to protobuf bytes
func (p *SparseAgentPayload) ToProtobufBytes() ([]byte, error) {
	return proto.Marshal(p.ToProto())
}

// NewSparseAgentPayload creates a new thing
func NewSparseAgentPayload(hostName, env string, t AnalyzedTransaction) *SparseAgentPayload {
	return &SparseAgentPayload{
		HostName:     hostName,
		Env:          env,
		Transactions: []AnalyzedTransaction{t},
	}
}